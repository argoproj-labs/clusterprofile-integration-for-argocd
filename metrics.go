package main

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	clusterinventory "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	secretOperationCreate   = "create"
	secretOperationUpdate   = "update"
	secretOperationDelete   = "delete"
	inventoryNamespaceLabel = "inventory_namespace"

	// Leave time to return the last snapshot if the cache read times out.
	metricsCollectionTimeout = 5 * time.Second
)

type inventoryMemberGroupKey struct {
	namespace string
	memberID  string
}

type namespaceResolutionKey struct {
	namespace  string
	resolution inventoryMemberResolution
}

type inventoryMemberConflictKey struct {
	namespace  string
	memberID   string
	resolution inventoryMemberResolution
}

type clusterProfileMetricsSnapshot struct {
	groupCounts        map[namespaceResolutionKey]float64
	conflictGroupSizes map[inventoryMemberConflictKey]float64
	invalidProfiles    map[string]float64
}

func emptyClusterProfileMetricsSnapshot() clusterProfileMetricsSnapshot {
	return clusterProfileMetricsSnapshot{
		groupCounts:        map[namespaceResolutionKey]float64{},
		conflictGroupSizes: map[inventoryMemberConflictKey]float64{},
		invalidProfiles:    map[string]float64{},
	}
}

type clusterProfileMetrics struct {
	reader client.Reader
	log    logr.Logger
	dryRun string

	inventoryMemberGroups            *prometheus.Desc
	inventoryMemberConflictGroupSize *prometheus.Desc
	invalidProfiles                  *prometheus.Desc
	secretChanges                    *prometheus.CounterVec
	collectionErrors                 prometheus.Counter

	snapshotMu sync.Mutex
	snapshot   clusterProfileMetricsSnapshot
}

func newClusterProfileMetrics(reader client.Reader, log logr.Logger, dryRun bool) *clusterProfileMetrics {
	metrics := &clusterProfileMetrics{
		reader: reader,
		log:    log,
		dryRun: strconv.FormatBool(dryRun),
		inventoryMemberGroups: prometheus.NewDesc(
			"argocd_clusterprofile_inventory_member_groups",
			"Number of active inventory member groups by namespace and selection resolution.",
			[]string{inventoryNamespaceLabel, "resolution"},
			nil,
		),
		inventoryMemberConflictGroupSize: prometheus.NewDesc(
			"argocd_clusterprofile_inventory_member_conflict_group_size",
			"Number of active ClusterProfiles in a duplicate or ambiguous inventory member ID group.",
			[]string{inventoryNamespaceLabel, "inventory_member_id", "resolution"},
			nil,
		),
		invalidProfiles: prometheus.NewDesc(
			"argocd_clusterprofile_inventory_member_id_invalid_profiles",
			"Number of active ClusterProfiles whose inventory member ID label is present but empty.",
			[]string{inventoryNamespaceLabel},
			nil,
		),
		secretChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "argocd_clusterprofile_secret_changes_total",
			Help: "Number of successful controller-issued Argo CD cluster Secret changes.",
		}, []string{inventoryNamespaceLabel, "operation", "dry_run"}),
		collectionErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "argocd_clusterprofile_inventory_collection_errors_total",
			Help: "Number of failures collecting ClusterProfile inventory state from the controller cache.",
		}),
		snapshot: emptyClusterProfileMetricsSnapshot(),
	}
	return metrics
}

func (m *clusterProfileMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.inventoryMemberGroups
	ch <- m.inventoryMemberConflictGroupSize
	ch <- m.invalidProfiles
	m.secretChanges.Describe(ch)
	m.collectionErrors.Describe(ch)
}

func (m *clusterProfileMetrics) Collect(ch chan<- prometheus.Metric) {
	snapshot := m.collectInventorySnapshot()
	for key, count := range snapshot.groupCounts {
		ch <- prometheus.MustNewConstMetric(
			m.inventoryMemberGroups,
			prometheus.GaugeValue,
			count,
			key.namespace,
			string(key.resolution),
		)
	}
	for key, count := range snapshot.conflictGroupSizes {
		ch <- prometheus.MustNewConstMetric(
			m.inventoryMemberConflictGroupSize,
			prometheus.GaugeValue,
			count,
			key.namespace,
			key.memberID,
			string(key.resolution),
		)
	}
	for namespace, count := range snapshot.invalidProfiles {
		ch <- prometheus.MustNewConstMetric(
			m.invalidProfiles,
			prometheus.GaugeValue,
			count,
			namespace,
		)
	}
	m.secretChanges.Collect(ch)
	m.collectionErrors.Collect(ch)
}

func (m *clusterProfileMetrics) collectInventorySnapshot() clusterProfileMetricsSnapshot {
	ctx, cancel := context.WithTimeout(context.Background(), metricsCollectionTimeout)
	defer cancel()

	profiles := &clusterinventory.ClusterProfileList{}
	if err := m.reader.List(ctx, profiles); err != nil {
		m.collectionErrors.Inc()
		m.log.Error(err, "unable to collect ClusterProfile inventory metrics; retaining last successful snapshot")
		m.snapshotMu.Lock()
		defer m.snapshotMu.Unlock()
		return m.snapshot
	}

	snapshot := buildClusterProfileMetricsSnapshot(profiles.Items)
	m.snapshotMu.Lock()
	m.snapshot = snapshot
	m.snapshotMu.Unlock()
	return snapshot
}

func buildClusterProfileMetricsSnapshot(
	items []clusterinventory.ClusterProfile,
) clusterProfileMetricsSnapshot {
	snapshot := emptyClusterProfileMetricsSnapshot()
	groups := map[inventoryMemberGroupKey][]*clusterinventory.ClusterProfile{}

	for i := range items {
		profile := &items[i]
		if !profile.DeletionTimestamp.IsZero() {
			continue
		}

		memberID, set := inventoryMemberID(profile)
		if memberID == "" {
			if set {
				snapshot.invalidProfiles[profile.Namespace]++
			}
			continue
		}
		key := inventoryMemberGroupKey{namespace: profile.Namespace, memberID: memberID}
		groups[key] = append(groups[key], profile)
	}

	for key, profiles := range groups {
		_, resolution := resolveInventoryMember(profiles)
		snapshot.groupCounts[namespaceResolutionKey{
			namespace:  key.namespace,
			resolution: resolution,
		}]++
		if resolution == inventoryMemberResolutionDuplicate || resolution == inventoryMemberResolutionAmbiguous {
			snapshot.conflictGroupSizes[inventoryMemberConflictKey{
				namespace:  key.namespace,
				memberID:   key.memberID,
				resolution: resolution,
			}] = float64(len(profiles))
		}
	}

	return snapshot
}

func (m *clusterProfileMetrics) recordSecretChange(namespace string, operation string) {
	if m == nil {
		return
	}
	m.secretChanges.WithLabelValues(namespace, operation, m.dryRun).Inc()
}
