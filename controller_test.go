package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/v3/common"
	appv1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	clusterinventory "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	testClusterName          = "test-cluster"
	testNamespace            = "default"
	testServer               = "https://test-cluster.example.com"
	testSecretName           = "cluster-test-cluster"
	testProviderName         = "secretreader"
	testProviderCommand      = "/plugins/secretreader/bin/secretreader-plugin"
	unconfiguredProviderName = "not-configured"
	trueValue                = "true"
	argocdNamespace          = "argocd"
	environmentLabel         = "environment"
	teamLabel                = "team"
	productionValue          = "production"
	stagingValue             = "staging"
	platformTeamValue        = "platform"
	managedByLabel           = "app.example.com/managed-by"
	teamANamespace           = "team-a"
	teamBNamespace           = "team-b"
	testClusterProfileUID    = "cluster-profile-uid"
)

type deleteConflictClient struct {
	client.Client
	deleteOptions client.DeleteOptions
}

func (c *deleteConflictClient) Delete(
	_ context.Context,
	object client.Object,
	options ...client.DeleteOption,
) error {
	c.deleteOptions = *(&client.DeleteOptions{}).ApplyOptions(options)
	return apierrors.NewConflict(
		schema.GroupResource{Resource: "secrets"},
		object.GetName(),
		errors.New("simulated cached-read race"),
	)
}

type updateCountingClient struct {
	client.Client
	updates int
}

func (c *updateCountingClient) Update(
	ctx context.Context,
	object client.Object,
	options ...client.UpdateOption,
) error {
	c.updates++
	return c.Client.Update(ctx, object, options...)
}

type patchConflictClient struct {
	client.Client
}

func (c *patchConflictClient) Patch(
	_ context.Context,
	object client.Object,
	_ client.Patch,
	_ ...client.PatchOption,
) error {
	return apierrors.NewConflict(
		schema.GroupResource{Resource: "secrets"},
		object.GetName(),
		errors.New("simulated cached-read patch race"),
	)
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterinventory.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

func profileKey() types.NamespacedName {
	return types.NamespacedName{Name: testClusterName, Namespace: testNamespace}
}

func profileRequest() reconcile.Request {
	return reconcile.Request{NamespacedName: profileKey()}
}

func secretKey(namespace string) types.NamespacedName {
	return types.NamespacedName{Name: testSecretName, Namespace: namespace}
}

func getSecret(t *testing.T, r *ClusterProfileReconciler, namespace string) *corev1.Secret {
	t.Helper()

	secret := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), secretKey(namespace), secret))
	return secret
}

func requireNoSecret(t *testing.T, r *ClusterProfileReconciler, key types.NamespacedName) {
	t.Helper()

	err := r.Get(context.Background(), key, &corev1.Secret{})
	require.True(t, apierrors.IsNotFound(err), "expected NotFound, got %v", err)
}

func updateProfile(
	t *testing.T,
	r *ClusterProfileReconciler,
	key types.NamespacedName,
	mutate func(*clusterinventory.ClusterProfile),
) {
	t.Helper()

	profile := &clusterinventory.ClusterProfile{}
	require.NoError(t, r.Get(context.Background(), key, profile))
	mutate(profile)
	require.NoError(t, r.Update(context.Background(), profile))
}

func newClusterProfile(
	labels map[string]string,
	providers ...clusterinventory.AccessProvider,
) *clusterinventory.ClusterProfile {
	return &clusterinventory.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
			UID:       testClusterProfileUID,
			Labels:    labels,
		},
		Status: clusterinventory.ClusterProfileStatus{AccessProviders: providers},
	}
}

func newBuiltinProviderClusterProfile(labels map[string]string) *clusterinventory.ClusterProfile {
	return newClusterProfile(labels, clusterinventory.AccessProvider{
		Name:    "argo-cd-builtin-gcp",
		Cluster: clientcmdv1.Cluster{Server: testServer},
	})
}

func newCustomProviderClusterProfile(labels map[string]string) *clusterinventory.ClusterProfile {
	return newClusterProfile(labels, clusterinventory.AccessProvider{
		Name:    testProviderName,
		Cluster: clientcmdv1.Cluster{Server: testServer},
	})
}

func clusterProfileOwnerReference(name string, uid types.UID) metav1.OwnerReference {
	controller := true
	blockOwnerDeletion := false
	return metav1.OwnerReference{
		APIVersion:         "multicluster.x-k8s.io/v1alpha1",
		Kind:               "ClusterProfile",
		Name:               name,
		UID:                uid,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}
}

func newControlledSecret(
	clusterProfile *clusterinventory.ClusterProfile,
	uid types.UID,
) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            testSecretName,
			Namespace:       clusterProfile.Namespace,
			UID:             uid,
			ResourceVersion: "42",
			Labels: map[string]string{
				clusterProfileNameKey:     clusterProfile.Name,
				common.LabelKeySecretType: common.LabelValueSecretTypeCluster,
			},
			OwnerReferences: []metav1.OwnerReference{
				clusterProfileOwnerReference(clusterProfile.Name, clusterProfile.UID),
			},
		},
		Data: map[string][]byte{
			secretDataNameKey:   []byte(clusterProfile.Name),
			secretDataServerKey: []byte(testServer),
			secretDataConfigKey: []byte(`{"previous":true}`),
		},
	}
}

func newUnownedSecret(
	namespace string,
	uid types.UID,
	labels map[string]string,
	owners []metav1.OwnerReference,
) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            testSecretName,
			Namespace:       namespace,
			UID:             uid,
			ResourceVersion: "42",
			Labels:          labels,
			OwnerReferences: owners,
		},
		Data: map[string][]byte{
			secretDataNameKey:   []byte("external-cluster"),
			secretDataServerKey: []byte("https://external.example.com"),
			secretDataConfigKey: []byte(`{"external":true}`),
		},
	}
}

func seedStaleClusterSecretData(secret *corev1.Secret) {
	secret.Data[secretDataNameKey] = []byte("stale-name")
	secret.Data[secretDataServerKey] = []byte("https://stale.example.com")
	secret.Data["project"] = []byte("stale-project")
	secret.Data["namespaces"] = []byte("stale-namespace")
	secret.Data["clusterResources"] = []byte(trueValue)
	secret.Data["shard"] = []byte("99")
	secret.Data["stale-auth-material"] = []byte("must-be-removed")
}

func requireExactTestClusterSecretData(t *testing.T, secret *corev1.Secret) appv1alpha1.ClusterConfig {
	t.Helper()

	require.Equal(t,
		[]string{secretDataConfigKey, secretDataNameKey, secretDataServerKey},
		slices.Sorted(maps.Keys(secret.Data)),
	)
	require.Equal(t, testClusterName, string(secret.Data[secretDataNameKey]))
	require.Equal(t, testServer, string(secret.Data[secretDataServerKey]))

	return requireClusterConfig(t, secret.Data)
}

func requireClusterConfig(t *testing.T, data map[string][]byte) appv1alpha1.ClusterConfig {
	t.Helper()

	var config appv1alpha1.ClusterConfig
	require.NoError(t, json.Unmarshal(data[secretDataConfigKey], &config))
	return config
}

func assertDeletePreconditions(t *testing.T, conflictClient *deleteConflictClient, secret *corev1.Secret) {
	t.Helper()

	require.NotNil(t, conflictClient.deleteOptions.Preconditions)
	require.NotNil(t, conflictClient.deleteOptions.Preconditions.UID)
	assert.Equal(t, secret.UID, *conflictClient.deleteOptions.Preconditions.UID)
	require.NotNil(t, conflictClient.deleteOptions.Preconditions.ResourceVersion)
	assert.Equal(t, secret.ResourceVersion, *conflictClient.deleteOptions.Preconditions.ResourceVersion)
}

func writeProvidersFile(t *testing.T) string {
	t.Helper()

	execConfig := map[string]any{
		"apiVersion":         "client.authentication.k8s.io/v1",
		"command":            testProviderCommand,
		"provideClusterInfo": true,
	}
	providerConfig := map[string]any{
		"providers": []map[string]any{
			{
				secretDataNameKey: testProviderName,
				"execConfig":      execConfig,
			},
		},
	}
	data, err := json.Marshal(providerConfig)
	require.NoError(t, err)

	return writeProviderConfigFile(t, data)
}

func writeProviderConfigFile(t *testing.T, data []byte) string {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "providers.json")
	require.NoError(t, err)
	_, err = file.Write(data)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	return file.Name()
}

func TestBuildRESTConfig(t *testing.T) {
	t.Run("uses the selected context and applies controller defaults", func(t *testing.T) {
		const hub, spoke = "hub", "spoke"
		config := clientcmdapi.Config{
			Clusters: map[string]*clientcmdapi.Cluster{
				hub:   {Server: "https://hub.example.com"},
				spoke: {Server: "https://spoke.example.com"},
			},
			Contexts: map[string]*clientcmdapi.Context{
				hub:   {Cluster: hub},
				spoke: {Cluster: spoke},
			},
			CurrentContext: spoke,
		}
		overrides := &clientcmd.ConfigOverrides{CurrentContext: hub}
		clientConfig := clientcmd.NewNonInteractiveClientConfig(config, "", overrides, nil)

		restConfig, err := buildRESTConfig(clientConfig)

		require.NoError(t, err)
		assert.Equal(t, "https://hub.example.com", restConfig.Host)
		version := common.GetVersion()
		assert.Equal(t,
			cliName+"/"+version.Version+" ("+version.Platform+")",
			restConfig.UserAgent,
		)
		assert.Equal(t, appv1alpha1.K8sClientConfigQPS, restConfig.QPS)
		assert.Equal(t, appv1alpha1.K8sClientConfigBurst, restConfig.Burst)
		assert.Equal(t, appv1alpha1.K8sServerSideTimeout, restConfig.Timeout)
	})

	t.Run("returns an error for an invalid client configuration", func(t *testing.T) {
		clientConfig := clientcmd.NewNonInteractiveClientConfig(
			clientcmdapi.Config{},
			"",
			&clientcmd.ConfigOverrides{},
			nil,
		)

		restConfig, err := buildRESTConfig(clientConfig)

		assert.Nil(t, restConfig)
		require.Error(t, err)
	})
}

func TestCacheSyncReadiness(t *testing.T) {
	readiness := &cacheSyncReadiness{}
	require.ErrorContains(t, readiness.Check(nil), "manager cache has not synced")
	assert.False(t, readiness.NeedLeaderElection())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- readiness.Start(ctx)
	}()
	require.Eventually(t, func() bool {
		return readiness.Check(nil) == nil
	}, time.Second, time.Millisecond)

	cancel()
	require.NoError(t, <-done)
	require.ErrorContains(t, readiness.Check(nil), "manager cache has not synced")
}

func TestGeneratedClusterProfileMetadata(t *testing.T) {
	t.Run("preserves name labels at the raw limit and bounds the next byte", func(t *testing.T) {
		atLimit := strings.Repeat("a", content.LabelValueMaxLength)

		assert.Equal(t, atLimit, clusterProfileNameLabelValue(atLimit))
		bounded := clusterProfileNameLabelValue(atLimit + "a")
		assert.Len(t, bounded, content.LabelValueMaxLength)
		assert.Empty(t, content.IsLabelValue(bounded))
	})

	t.Run("preserves Secret names at the raw limit and bounds longer valid profiles", func(t *testing.T) {
		atLimit := strings.Repeat("a", content.DNS1123SubdomainMaxLength-len("cluster-"))

		assert.Equal(t, "cluster-"+atLimit, clusterProfileSecretName(atLimit))
		for _, profileName := range []string{
			atLimit + "a",
			strings.Repeat("m", content.DNS1123SubdomainMaxLength),
		} {
			bounded := clusterProfileSecretName(profileName)
			assert.Len(t, bounded, content.DNS1123SubdomainMaxLength)
			assert.Empty(t, content.IsDNS1123Subdomain(bounded))
		}
	})

	t.Run("keeps bounded values valid when truncation lands on punctuation", func(t *testing.T) {
		// Both names are sized so the retained prefix ends on punctuation that has to be trimmed,
		// leaving the bounded value one byte short of its limit.
		nameProfile := strings.Repeat("a", 29) + "-" + strings.Repeat("b", 40)
		secretProfile := strings.Repeat("a", 204) + "." + strings.Repeat("b", 41)

		labelValue := clusterProfileNameLabelValue(nameProfile)
		secretName := clusterProfileSecretName(secretProfile)

		assert.Equal(t, strings.Repeat("a", 29)+"_9ef65b510e53c27b53f8b72e95994ff5", labelValue)
		assert.Equal(t,
			"clusterprofile-"+strings.Repeat("a", 204)+"-fe3cdbf4f4591be680e1102a7cd0f630",
			secretName,
		)
		assert.Empty(t, content.IsLabelValue(labelValue))
		assert.Empty(t, content.IsDNS1123Subdomain(secretName))
	})

	t.Run("does not collide for long names with the same readable prefix", func(t *testing.T) {
		prefix := strings.Repeat("a", 245)
		first := prefix + "b"
		second := prefix + "c"

		assert.NotEqual(t, clusterProfileSecretName(first), clusterProfileSecretName(second))
		assert.NotEqual(t, clusterProfileNameLabelValue(first), clusterProfileNameLabelValue(second))
	})

	t.Run("keeps bounded encodings disjoint from every raw encoding", func(t *testing.T) {
		longSecretProfile := strings.Repeat("s", 246)
		boundedLookalikeSecretName := "cluster-" + strings.Repeat("s", 212) +
			"-c487d7cd89959dbc1df6f5deec5584b5"
		rawSecretCollisionProfile := strings.TrimPrefix(boundedLookalikeSecretName, "cluster-")
		assert.Equal(t, boundedLookalikeSecretName, clusterProfileSecretName(rawSecretCollisionProfile))
		assert.Equal(t,
			"clusterprofile-"+strings.Repeat("s", 205)+"-c487d7cd89959dbc1df6f5deec5584b5",
			clusterProfileSecretName(longSecretProfile),
		)
		longNameProfile := strings.Repeat("l", 64)
		rawNameCollisionProfile := strings.Repeat("l", 30) + "-6714e95219c67c4cda7eeff21b662ca5"
		assert.Equal(t, rawNameCollisionProfile, clusterProfileNameLabelValue(rawNameCollisionProfile))
		assert.Equal(t,
			strings.Repeat("l", 30)+"_6714e95219c67c4cda7eeff21b662ca5",
			clusterProfileNameLabelValue(longNameProfile),
		)
	})

	t.Run("retains short metadata values verbatim", func(t *testing.T) {
		assert.Equal(t, testSecretName, clusterProfileSecretName(testClusterName))
		assert.Equal(t, testClusterName, clusterProfileNameLabelValue(testClusterName))
	})
}

func TestEffectiveAccessProviderSelection(t *testing.T) {
	profile := &clusterinventory.ClusterProfile{
		Status: clusterinventory.ClusterProfileStatus{
			CredentialProviders: []clusterinventory.CredentialProvider{
				{
					Name:    testProviderName,
					Cluster: clientcmdv1.Cluster{Server: "https://deprecated.example.com"},
				},
				{
					Name:    "second",
					Cluster: clientcmdv1.Cluster{Server: "https://second.example.com"},
				},
			},
			AccessProviders: []clusterinventory.AccessProvider{
				{
					Name:    testProviderName,
					Cluster: clientcmdv1.Cluster{Server: testServer},
				},
			},
		},
	}

	t.Run("overrides a deprecated credential provider of the same name", func(t *testing.T) {
		effective := effectiveAccessProviders(profile)

		require.Len(t, effective, 2)
		assert.Equal(t, testServer, effective[testProviderName].Cluster.Server)
	})

	t.Run("selects the first configured provider", func(t *testing.T) {
		selected := selectCustomAccessProvider(profile, &access.Config{Providers: []access.Provider{
			{Name: "second"},
			{Name: testProviderName},
		}})

		require.NotNil(t, selected)
		assert.Equal(t, "second", selected.Name)
	})

	t.Run("selects the only configured provider", func(t *testing.T) {
		selected := selectCustomAccessProvider(profile, &access.Config{Providers: []access.Provider{
			{Name: testProviderName},
		}})

		require.NotNil(t, selected)
		assert.Equal(t, testProviderName, selected.Name)
		assert.Equal(t, testServer, selected.Cluster.Server)
	})
}

func TestRenderFailureRevocation(t *testing.T) {
	scheme := newTestScheme(t)
	providersFile := writeProvidersFile(t)

	newFixture := func(t *testing.T, labels map[string]string) (*ClusterProfileReconciler, *corev1.Secret) {
		t.Helper()
		reconciler := &ClusterProfileReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(newCustomProviderClusterProfile(labels)).Build(),
			Log:                        logr.Discard(),
			Scheme:                     scheme,
			ClusterProfileProviderFile: providersFile,
		}
		require.NoError(t, reconciler.loadClusterProfileProviderFile())
		_, err := reconciler.Reconcile(context.Background(), profileRequest())
		require.NoError(t, err)
		return reconciler, getSecret(t, reconciler, testNamespace)
	}

	t.Run("retains last-known-good data during a local provider configuration outage", func(t *testing.T) {
		reconciler, original := newFixture(t, map[string]string{environmentLabel: productionValue})
		reconciler.AccessProviders = nil

		_, err := reconciler.Reconcile(context.Background(), profileRequest())

		require.ErrorContains(t, err, "AccessProviders not initialized")
		preserved := getSecret(t, reconciler, testNamespace)
		assert.Equal(t, original.ResourceVersion, preserved.ResourceVersion)
		assert.Equal(t, original.Data, preserved.Data)
		assert.Equal(t, original.Labels, preserved.Labels)
		assert.Equal(t, original.Annotations, preserved.Annotations)
	})

	t.Run("retains provider A when unrelated provider B is added during an outage", func(t *testing.T) {
		reconciler, original := newFixture(t, nil)
		updateProfile(t, reconciler, profileKey(), func(profile *clusterinventory.ClusterProfile) {
			profile.Status.AccessProviders = append(profile.Status.AccessProviders,
				clusterinventory.AccessProvider{
					Name:    "unrelated",
					Cluster: clientcmdv1.Cluster{Server: "https://unrelated.example.com"},
				})
		})
		reconciler.AccessProviders = nil

		_, err := reconciler.Reconcile(context.Background(), profileRequest())

		require.Error(t, err)
		preserved := getSecret(t, reconciler, testNamespace)
		assert.Equal(t, original.Data, preserved.Data)
		assert.Equal(t, original.Annotations, preserved.Annotations)
	})

	t.Run("retains credentials when the advertised provider rotates its CA", func(t *testing.T) {
		reconciler, original := newFixture(t, nil)
		updateProfile(t, reconciler, profileKey(), func(profile *clusterinventory.ClusterProfile) {
			profile.Status.AccessProviders[0].Cluster.CertificateAuthorityData = []byte("rotated-ca")
		})
		reconciler.AccessProviders = nil

		_, err := reconciler.Reconcile(context.Background(), profileRequest())

		require.Error(t, err)
		preserved := getSecret(t, reconciler, testNamespace)
		assert.Equal(t, original.Data, preserved.Data)
	})

	t.Run("revokes credentials when the selected provider changes", func(t *testing.T) {
		reconciler, _ := newFixture(t, nil)
		updateProfile(t, reconciler, profileKey(), func(profile *clusterinventory.ClusterProfile) {
			profile.Status.AccessProviders = []clusterinventory.AccessProvider{
				{
					Name:    unconfiguredProviderName,
					Cluster: clientcmdv1.Cluster{Server: "https://replacement.example.com"},
				},
			}
		})
		reconciler.AccessProviders = nil

		_, err := reconciler.Reconcile(context.Background(), profileRequest())

		require.Error(t, err)
		requireNoSecret(t, reconciler, secretKey(testNamespace))
	})

	t.Run("updates ApplicationSet labels without re-rendering credentials", func(t *testing.T) {
		reconciler, original := newFixture(t, map[string]string{
			environmentLabel: productionValue,
			"obsolete":       "remove-me",
		})
		originalPayloadFingerprint := original.Annotations[secretPayloadFingerprintAnnotation]
		secretWithExternalAnnotation := getSecret(t, reconciler, testNamespace)
		secretWithExternalAnnotation.Annotations["example.com/retained"] = trueValue
		require.NoError(t, reconciler.Update(context.Background(), secretWithExternalAnnotation))
		updateProfile(t, reconciler, profileKey(), func(profile *clusterinventory.ClusterProfile) {
			profile.Labels = map[string]string{
				environmentLabel: stagingValue,
				teamLabel:        platformTeamValue,
			}
		})
		reconciler.AccessProviders = nil

		_, err := reconciler.Reconcile(context.Background(), profileRequest())

		require.Error(t, err)
		updated := getSecret(t, reconciler, testNamespace)
		assert.Equal(t, original.Data, updated.Data)
		assert.Equal(t, original.OwnerReferences, updated.OwnerReferences)
		assert.Equal(t, stagingValue, updated.Labels[environmentLabel])
		assert.Equal(t, platformTeamValue, updated.Labels[teamLabel])
		assert.NotContains(t, updated.Labels, "obsolete")
		assert.Equal(t, common.LabelValueSecretTypeCluster, updated.Labels[common.LabelKeySecretType])
		assert.Equal(t, testClusterName, updated.Labels[clusterProfileNameKey])
		assert.Equal(t, trueValue, updated.Annotations["example.com/retained"])
		assert.Equal(t, testProviderName, updated.Annotations[secretAccessProviderAnnotation])
		// The payload fingerprint covers data only, so converging labels leaves it intact.
		assert.Equal(t, originalPayloadFingerprint, updated.Annotations[secretPayloadFingerprintAnnotation])
	})

	t.Run("retries when the metadata-only optimistic patch conflicts", func(t *testing.T) {
		reconciler, original := newFixture(t, map[string]string{environmentLabel: productionValue})
		updateProfile(t, reconciler, profileKey(), func(profile *clusterinventory.ClusterProfile) {
			profile.Labels[environmentLabel] = stagingValue
		})
		reconciler.AccessProviders = nil
		reconciler.Client = &patchConflictClient{Client: reconciler.Client}

		_, err := reconciler.Reconcile(context.Background(), profileRequest())

		require.Error(t, err)
		assert.True(t, apierrors.IsConflict(err))
		preserved := getSecret(t, reconciler, testNamespace)
		assert.Equal(t, productionValue, preserved.Labels[environmentLabel])
		assert.Equal(t, original.Annotations, preserved.Annotations)
	})

	t.Run("preserves an unprovable payload after the stamped provider changes", func(t *testing.T) {
		reconciler, _ := newFixture(t, nil)
		secret := getSecret(t, reconciler, testNamespace)
		secret.Data[secretDataServerKey] = []byte("https://external-writer.example.com")
		require.NoError(t, reconciler.Update(context.Background(), secret))
		updateProfile(t, reconciler, profileKey(), func(profile *clusterinventory.ClusterProfile) {
			profile.Status.AccessProviders[0].Name = unconfiguredProviderName
		})
		reconciler.AccessProviders = nil

		_, err := reconciler.Reconcile(context.Background(), profileRequest())

		require.Error(t, err)
		preserved := getSecret(t, reconciler, testNamespace)
		assert.Equal(t, secret.ResourceVersion, preserved.ResourceVersion)
		assert.Equal(t, secret.Data, preserved.Data)
	})

	t.Run("preserves an unstamped Secret conservatively", func(t *testing.T) {
		for _, annotation := range []string{
			secretAccessProviderAnnotation,
			secretPayloadFingerprintAnnotation,
		} {
			t.Run("without "+annotation, func(t *testing.T) {
				reconciler, _ := newFixture(t, nil)
				secret := getSecret(t, reconciler, testNamespace)
				delete(secret.Annotations, annotation)
				require.NoError(t, reconciler.Update(context.Background(), secret))
				updateProfile(t, reconciler, profileKey(), func(profile *clusterinventory.ClusterProfile) {
					profile.Status.AccessProviders[0].Name = unconfiguredProviderName
				})
				reconciler.AccessProviders = nil

				_, err := reconciler.Reconcile(context.Background(), profileRequest())

				require.Error(t, err)
				preserved := getSecret(t, reconciler, testNamespace)
				assert.Equal(t, secret.ResourceVersion, preserved.ResourceVersion)
				assert.Equal(t, secret.Data, preserved.Data)
			})
		}
	})

	t.Run("preserves a foreign Secret without partially adopting it", func(t *testing.T) {
		profile := newCustomProviderClusterProfile(nil)
		profile.Status.AccessProviders[0].Name = unconfiguredProviderName
		foreignSecret := newUnownedSecret(
			testNamespace,
			"foreign-secret-uid",
			map[string]string{managedByLabel: "someone-else"},
			nil,
		)
		before := foreignSecret.DeepCopy()
		reconciler := &ClusterProfileReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile, foreignSecret).Build(),
			Log:    logr.Discard(),
			Scheme: scheme,
		}
		_, err := reconciler.Reconcile(context.Background(), profileRequest())

		require.Error(t, err)
		assert.Equal(t, before, getSecret(t, reconciler, testNamespace))
	})

	t.Run("uses UID and resourceVersion preconditions when obsolete deletion races", func(t *testing.T) {
		reconciler, _ := newFixture(t, nil)
		secret := getSecret(t, reconciler, testNamespace)
		secret.UID = "fingerprinted-secret-uid"
		require.NoError(t, reconciler.Update(context.Background(), secret))
		updateProfile(t, reconciler, profileKey(), func(profile *clusterinventory.ClusterProfile) {
			profile.Status.AccessProviders[0].Name = unconfiguredProviderName
		})
		conflictClient := &deleteConflictClient{Client: reconciler.Client}
		reconciler.Client = conflictClient
		reconciler.AccessProviders = nil

		_, err := reconciler.Reconcile(context.Background(), profileRequest())

		require.Error(t, err)
		assert.True(t, apierrors.IsConflict(err))
		assertDeletePreconditions(t, conflictClient, secret)
		assert.Equal(t, secret.UID, getSecret(t, reconciler, testNamespace).UID)
	})

	t.Run("backfills the render annotations and preserves unrelated ones", func(t *testing.T) {
		profile := newCustomProviderClusterProfile(nil)
		secret := newControlledSecret(profile, "existing-secret-uid")
		secret.Annotations = map[string]string{"example.com/retained": trueValue}
		reconciler := &ClusterProfileReconciler{
			Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile, secret).Build(),
			Log:                        logr.Discard(),
			Scheme:                     scheme,
			ClusterProfileProviderFile: providersFile,
		}
		require.NoError(t, reconciler.loadClusterProfileProviderFile())

		_, err := reconciler.Reconcile(context.Background(), profileRequest())

		require.NoError(t, err)
		updated := getSecret(t, reconciler, testNamespace)
		assert.Equal(t, trueValue, updated.Annotations["example.com/retained"])
		assert.Equal(t, testProviderName, updated.Annotations[secretAccessProviderAnnotation])
		assert.NotEmpty(t, updated.Annotations[secretPayloadFingerprintAnnotation])
	})
}

func TestClusterProfileReconciler(t *testing.T) {
	scheme := newTestScheme(t)

	t.Run("Reconcile", func(t *testing.T) {
		t.Run("should create a secret when a new ClusterProfile is created", func(t *testing.T) {
			providersFile := writeProvidersFile(t)

			clusterProfile := newCustomProviderClusterProfile(nil)
			clusterProfile.Status.AccessProviders[0].Cluster.Extensions = []clientcmdv1.NamedExtension{
				{
					Name: "client.authentication.k8s.io/exec",
					Extension: runtime.RawExtension{
						Raw: []byte(`{"clusterName":"test-cluster"}`),
					},
				},
			}
			r := &ClusterProfileReconciler{
				Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:                        logr.Discard(),
				Scheme:                     scheme,
				ClusterProfileProviderFile: providersFile,
			}
			require.NoError(t, r.loadClusterProfileProviderFile())

			_, err := r.Reconcile(context.Background(), profileRequest())

			require.NoError(t, err)
			secret := getSecret(t, r, testNamespace)
			assert.Equal(t, testSecretName, secret.Name)
			assert.Equal(t, testNamespace, secret.Namespace)
			assert.Equal(t, []metav1.OwnerReference{
				clusterProfileOwnerReference(testClusterName, testClusterProfileUID),
			}, secret.OwnerReferences)
			assert.Equal(t, "cluster", secret.Labels["argocd.argoproj.io/secret-type"])
			assert.Equal(t, testClusterName, secret.Labels["argocd.argoproj.io/cluster-profile-name"])
			assert.Equal(t, testClusterName, secret.Annotations["argocd.argoproj.io/cluster-profile-name"])
			assert.Equal(t, testClusterName, string(secret.Data[secretDataNameKey]))
			assert.Equal(t, testServer, string(secret.Data[secretDataServerKey]))

			execProviderConfig := requireClusterConfig(t, secret.Data).ExecProviderConfig
			require.NotNil(t, execProviderConfig)
			assert.Equal(t, "client.authentication.k8s.io/v1", execProviderConfig.APIVersion)
			assert.Equal(t, testProviderCommand, execProviderConfig.Command)
			assert.True(t, execProviderConfig.ProvideClusterInfo)
			require.NotNil(t, execProviderConfig.Config)
			assert.JSONEq(t, `{"clusterName":"`+testClusterName+`"}`, string(execProviderConfig.Config.Raw))
		})

		t.Run("should not retain profile-sourced args between reconciles", func(t *testing.T) {
			execConfig := map[string]any{
				"apiVersion": "client.authentication.k8s.io/v1",
				"command":    testProviderCommand,
			}
			providerConfig := map[string]any{
				"providers": []map[string]any{
					{
						secretDataNameKey:             testProviderName,
						"execConfig":                  execConfig,
						"profileSourcedCLIArgsPolicy": access.ProfileSourcedCLIArgsPolicyAppend,
					},
				},
			}
			data, err := json.Marshal(providerConfig)
			require.NoError(t, err)
			providersFile := writeProviderConfigFile(t, data)

			clusterProfile := newCustomProviderClusterProfile(nil)
			clusterProfile.Status.AccessProviders[0].Cluster.Extensions = []clientcmdv1.NamedExtension{
				{
					Name: "clusterprofiles.multicluster.x-k8s.io/exec/additional-args",
					Extension: runtime.RawExtension{
						Raw: []byte(`["--cluster", "{{ .ClusterProfileName }}"]`),
					},
				},
			}
			r := &ClusterProfileReconciler{
				Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:                        logr.Discard(),
				Scheme:                     scheme,
				ClusterProfileProviderFile: providersFile,
			}
			require.NoError(t, r.loadClusterProfileProviderFile())

			_, err = r.Reconcile(context.Background(), profileRequest())
			require.NoError(t, err)
			_, err = r.Reconcile(context.Background(), profileRequest())
			require.NoError(t, err)

			secret := getSecret(t, r, testNamespace)
			execProviderConfig := requireClusterConfig(t, secret.Data).ExecProviderConfig
			require.NotNil(t, execProviderConfig)
			assert.Equal(t, []string{"--cluster", testClusterName}, execProviderConfig.Args)
		})

		t.Run("should create a secret for every supported built-in cloud provider", func(t *testing.T) {
			for _, cloudProvider := range []string{"aws", "azure", "gcp"} {
				t.Run(cloudProvider, func(t *testing.T) {
					clusterProfile := newBuiltinProviderClusterProfile(nil)
					clusterProfile.Status.AccessProviders[0].Name = "argo-cd-builtin-" + cloudProvider
					r := &ClusterProfileReconciler{
						Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
						Log:    logr.Discard(),
						Scheme: scheme,
					}

					_, err := r.Reconcile(context.Background(), profileRequest())

					require.NoError(t, err)
					secret := getSecret(t, r, testNamespace)
					execProviderConfig := requireClusterConfig(t, secret.Data).ExecProviderConfig
					require.NotNil(t, execProviderConfig)
					assert.Equal(t, "argocd-k8s-auth", execProviderConfig.Command)
					assert.Equal(t, []string{cloudProvider}, execProviderConfig.Args)
				})
			}
		})

		t.Run("should not create a secret for an unsupported built-in access provider", func(t *testing.T) {
			for _, providerName := range []string{
				"argo-cd-builtin-",
				"argo-cd-builtin-GCP",
				"argo-cd-builtin-unknown",
			} {
				t.Run(providerName, func(t *testing.T) {
					clusterProfile := newBuiltinProviderClusterProfile(nil)
					clusterProfile.Status.AccessProviders[0].Name = providerName
					r := &ClusterProfileReconciler{
						Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
						Log:    logr.Discard(),
						Scheme: scheme,
					}

					_, err := r.Reconcile(context.Background(), profileRequest())

					require.ErrorContains(t, err, "unsupported built-in access provider")
					requireNoSecret(t, r, secretKey(testNamespace))
				})
			}
		})

		t.Run("should not create a secret when the access provider has an empty cluster server", func(t *testing.T) {
			t.Run("built-in provider", func(t *testing.T) {
				clusterProfile := newBuiltinProviderClusterProfile(nil)
				clusterProfile.Status.AccessProviders[0].Cluster.Server = " \t"
				r := &ClusterProfileReconciler{
					Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
					Log:    logr.Discard(),
					Scheme: scheme,
				}

				_, err := r.Reconcile(context.Background(), profileRequest())

				require.ErrorContains(t, err, "has an empty cluster server")
				requireNoSecret(t, r, secretKey(testNamespace))
			})

			t.Run("custom provider", func(t *testing.T) {
				clusterProfile := newCustomProviderClusterProfile(nil)
				clusterProfile.Status.AccessProviders[0].Cluster.Server = ""
				r := &ClusterProfileReconciler{
					Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
					Log:                        logr.Discard(),
					Scheme:                     scheme,
					ClusterProfileProviderFile: writeProvidersFile(t),
				}
				require.NoError(t, r.loadClusterProfileProviderFile())

				_, err := r.Reconcile(context.Background(), profileRequest())

				require.ErrorContains(t, err, "has an empty cluster server")
				requireNoSecret(t, r, secretKey(testNamespace))
			})
		})

		t.Run("should preserve the cluster connection fields of the access provider", func(t *testing.T) {
			connection := clientcmdv1.Cluster{
				Server:                   testServer,
				TLSServerName:            "api.internal.example.com",
				InsecureSkipTLSVerify:    true,
				CertificateAuthorityData: []byte("test-ca-data"),
				ProxyURL:                 "socks5://proxy.example.com:1080",
				DisableCompression:       true,
			}
			assertConnection := func(t *testing.T, secret *corev1.Secret) {
				t.Helper()

				assert.Equal(t, testServer, string(secret.Data[secretDataServerKey]))
				config := requireClusterConfig(t, secret.Data)
				assert.Equal(t, connection.InsecureSkipTLSVerify, config.Insecure)
				assert.Equal(t, connection.TLSServerName, config.ServerName)
				assert.Equal(t, connection.CertificateAuthorityData, config.CAData)
				assert.Equal(t, connection.ProxyURL, config.ProxyUrl)
				assert.Equal(t, connection.DisableCompression, config.DisableCompression)
			}

			t.Run("built-in provider", func(t *testing.T) {
				clusterProfile := newBuiltinProviderClusterProfile(nil)
				clusterProfile.Status.AccessProviders[0].Cluster = connection
				r := &ClusterProfileReconciler{
					Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
					Log:    logr.Discard(),
					Scheme: scheme,
				}

				_, err := r.Reconcile(context.Background(), profileRequest())

				require.NoError(t, err)
				assertConnection(t, getSecret(t, r, testNamespace))
			})

			t.Run("custom provider", func(t *testing.T) {
				clusterProfile := newCustomProviderClusterProfile(nil)
				clusterProfile.Status.AccessProviders[0].Cluster = connection
				r := &ClusterProfileReconciler{
					Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
					Log:                        logr.Discard(),
					Scheme:                     scheme,
					ClusterProfileProviderFile: writeProvidersFile(t),
				}
				require.NoError(t, r.loadClusterProfileProviderFile())

				_, err := r.Reconcile(context.Background(), profileRequest())

				require.NoError(t, err)
				assertConnection(t, getSecret(t, r, testNamespace))
			})
		})

		t.Run("should not create a builtin secret from deprecated CredentialProviders", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(nil)
			clusterProfile.Status.CredentialProviders = clusterProfile.Status.AccessProviders
			clusterProfile.Status.AccessProviders = nil
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}

			_, err := r.Reconcile(context.Background(), profileRequest())

			require.Error(t, err)
			requireNoSecret(t, r, secretKey(testNamespace))
		})

		t.Run("should propagate ClusterProfile labels to the generated secret", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(map[string]string{
				environmentLabel: productionValue,
				teamLabel:        platformTeamValue,
			})
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}

			_, err := r.Reconcile(context.Background(), profileRequest())

			require.NoError(t, err)
			secret := getSecret(t, r, testNamespace)
			assert.Equal(t, productionValue, secret.Labels[environmentLabel])
			assert.Equal(t, platformTeamValue, secret.Labels[teamLabel])
			assert.Equal(t, common.LabelValueSecretTypeCluster, secret.Labels[common.LabelKeySecretType])
			assert.Equal(t, testClusterName, secret.Labels[clusterProfileNameKey])
		})

		t.Run("should protect controller-owned secret labels from ClusterProfile labels", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(map[string]string{
				common.LabelKeySecretType: "not-cluster",
				clusterProfileNameKey:     "other-name",
				environmentLabel:          productionValue,
			})
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}

			_, err := r.Reconcile(context.Background(), profileRequest())

			require.NoError(t, err)
			secret := getSecret(t, r, testNamespace)
			assert.Equal(t, productionValue, secret.Labels[environmentLabel])
			assert.Equal(t, common.LabelValueSecretTypeCluster, secret.Labels[common.LabelKeySecretType])
			assert.Equal(t, testClusterName, secret.Labels[clusterProfileNameKey])
		})

		t.Run("should update secret labels when ClusterProfile labels change", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(map[string]string{
				environmentLabel: stagingValue,
				teamLabel:        platformTeamValue,
			})
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}

			_, err := r.Reconcile(context.Background(), profileRequest())
			require.NoError(t, err)

			updateProfile(t, r, profileKey(), func(profile *clusterinventory.ClusterProfile) {
				profile.Labels = map[string]string{environmentLabel: productionValue}
			})

			_, err = r.Reconcile(context.Background(), profileRequest())

			require.NoError(t, err)
			secret := getSecret(t, r, testNamespace)
			assert.Equal(t, productionValue, secret.Labels[environmentLabel])
			assert.NotContains(t, secret.Labels, teamLabel)
			assert.Equal(t, common.LabelValueSecretTypeCluster, secret.Labels[common.LabelKeySecretType])
			assert.Equal(t, testClusterName, secret.Labels[clusterProfileNameKey])
		})

		t.Run("should update the secret when the ClusterProfile is updated", func(t *testing.T) {
			providersFile := writeProvidersFile(t)

			clusterProfile := newCustomProviderClusterProfile(nil)
			r := &ClusterProfileReconciler{
				Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:                        logr.Discard(),
				Scheme:                     scheme,
				ClusterProfileProviderFile: providersFile,
			}
			require.NoError(t, r.loadClusterProfileProviderFile())

			_, err := r.Reconcile(context.Background(), profileRequest())
			require.NoError(t, err)

			updateProfile(t, r, profileKey(), func(profile *clusterinventory.ClusterProfile) {
				profile.Status.AccessProviders[0].Cluster.Server = "https://updated-cluster.example.com"
			})

			_, err = r.Reconcile(context.Background(), profileRequest())
			require.NoError(t, err)

			secret := getSecret(t, r, testNamespace)
			assert.Equal(t, "https://updated-cluster.example.com", string(secret.Data[secretDataServerKey]))
		})

		t.Run("should not return an error if the ClusterProfile is not found", func(t *testing.T) {
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			res, err := r.Reconcile(context.Background(), profileRequest())

			require.NoError(t, err)
			assert.Equal(t, reconcile.Result{}, res)
		})

		t.Run("should do nothing while the ClusterProfile is being deleted", func(t *testing.T) {
			now := metav1.NewTime(time.Now())
			clusterProfile := newBuiltinProviderClusterProfile(nil)
			clusterProfile.DeletionTimestamp = &now
			clusterProfile.Finalizers = []string{"example.com/unrelated"}
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}

			_, err := r.Reconcile(context.Background(), profileRequest())

			// The secret's owner reference leaves the cleanup to the garbage collector.
			require.NoError(t, err)
			requireNoSecret(t, r, secretKey(testNamespace))
		})

		t.Run("should reject persisted Secrets without current provenance", func(t *testing.T) {
			testCases := []struct {
				name            string
				labels          map[string]string
				ownerReferences []metav1.OwnerReference
			}{
				{
					name: "ownerless manual Secret",
					labels: map[string]string{
						managedByLabel: "human",
					},
				},
				{
					name: "generated-looking labels without an owner",
					labels: map[string]string{
						clusterProfileNameKey:     testClusterName,
						common.LabelKeySecretType: common.LabelValueSecretTypeCluster,
					},
				},
				{
					name: "matching owner identity with a stale UID",
					labels: map[string]string{
						clusterProfileNameKey:     testClusterName,
						common.LabelKeySecretType: common.LabelValueSecretTypeCluster,
					},
					ownerReferences: []metav1.OwnerReference{
						clusterProfileOwnerReference(testClusterName, "stale-cluster-profile-uid"),
					},
				},
				{
					name: "another ClusterProfile as the controller",
					labels: map[string]string{
						managedByLabel: "foreign-controller",
					},
					ownerReferences: []metav1.OwnerReference{
						clusterProfileOwnerReference("another-cluster", "another-uid"),
					},
				},
			}

			for _, testCase := range testCases {
				t.Run(testCase.name, func(t *testing.T) {
					clusterProfile := newBuiltinProviderClusterProfile(nil)
					clusterProfile.Namespace = argocdNamespace
					secret := newUnownedSecret(
						argocdNamespace,
						"persisted-secret-uid",
						testCase.labels,
						testCase.ownerReferences,
					)
					before := secret.DeepCopy()
					r := &ClusterProfileReconciler{
						Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
						Log:    logr.Discard(),
						Scheme: scheme,
					}
					req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clusterProfile)}

					_, err := r.Reconcile(context.Background(), req)

					require.ErrorContains(t, err, "refusing to mutate Secret")
					assert.Equal(t, before, getSecret(t, r, argocdNamespace))
				})
			}
		})

		t.Run("should repair a Secret controlled by the current ClusterProfile UID", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(nil)
			clusterProfile.Namespace = argocdNamespace
			secret := newControlledSecret(clusterProfile, "current-secret-uid")
			secret.Labels = map[string]string{"app.example.com/stale": trueValue}
			seedStaleClusterSecretData(secret)
			countingClient := &updateCountingClient{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
			}
			r := &ClusterProfileReconciler{
				Client: countingClient,
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clusterProfile)}

			_, err := r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			repairedSecret := getSecret(t, r, argocdNamespace)
			assert.Equal(t, secret.UID, repairedSecret.UID)
			assert.Equal(t, common.LabelValueSecretTypeCluster, repairedSecret.Labels[common.LabelKeySecretType])
			assert.Equal(t, testClusterName, repairedSecret.Labels[clusterProfileNameKey])
			assert.NotContains(t, repairedSecret.Labels, "app.example.com/stale")
			config := requireExactTestClusterSecretData(t, repairedSecret)
			require.NotNil(t, config.ExecProviderConfig)
			assert.Equal(t, "argocd-k8s-auth", config.ExecProviderConfig.Command)
			assert.Equal(t, []string{"gcp"}, config.ExecProviderConfig.Args)
			assert.True(t, metav1.IsControlledBy(repairedSecret, clusterProfile))
			repairedResourceVersion := repairedSecret.ResourceVersion
			require.Equal(t, 1, countingClient.updates)

			_, err = r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			assert.Equal(t, 1, countingClient.updates)
			stableSecret := getSecret(t, r, argocdNamespace)
			assert.Equal(t, repairedResourceVersion, stableSecret.ResourceVersion)
			requireExactTestClusterSecretData(t, stableSecret)
		})

		t.Run("should replace stale provider data on a Secret owned by the same ClusterProfile UID", func(t *testing.T) {
			providersFile := writeProvidersFile(t)
			clusterProfile := newCustomProviderClusterProfile(nil)
			secret := newControlledSecret(clusterProfile, "custom-secret-uid")
			seedStaleClusterSecretData(secret)
			countingClient := &updateCountingClient{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
			}
			r := &ClusterProfileReconciler{
				Client:                     countingClient,
				Log:                        logr.Discard(),
				Scheme:                     scheme,
				ClusterProfileProviderFile: providersFile,
			}
			require.NoError(t, r.loadClusterProfileProviderFile())

			_, err := r.Reconcile(context.Background(), profileRequest())

			require.NoError(t, err)
			require.Equal(t, 1, countingClient.updates)
			repairedSecret := getSecret(t, r, testNamespace)
			config := requireExactTestClusterSecretData(t, repairedSecret)
			require.NotNil(t, config.ExecProviderConfig)
			assert.Equal(t, testProviderCommand, config.ExecProviderConfig.Command)
			repairedResourceVersion := repairedSecret.ResourceVersion

			_, err = r.Reconcile(context.Background(), profileRequest())

			require.NoError(t, err)
			assert.Equal(t, 1, countingClient.updates)
			stableSecret := getSecret(t, r, testNamespace)
			assert.Equal(t, repairedResourceVersion, stableSecret.ResourceVersion)
			requireExactTestClusterSecretData(t, stableSecret)
		})

		t.Run("should succeed idempotently when no access provider or Secret exists", func(t *testing.T) {
			clusterProfile := newClusterProfile(nil)
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}

			_, err := r.Reconcile(context.Background(), profileRequest())
			require.NoError(t, err)
			_, err = r.Reconcile(context.Background(), profileRequest())
			require.NoError(t, err)

			requireNoSecret(t, r, secretKey(testNamespace))
		})

		t.Run("should recreate a revoked Secret owned by the same ClusterProfile UID after recovery", func(t *testing.T) {
			clusterProfile := newClusterProfile(nil)
			secret := newControlledSecret(clusterProfile, "revoked-secret-uid")
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}

			_, err := r.Reconcile(context.Background(), profileRequest())
			require.NoError(t, err)
			requireNoSecret(t, r, secretKey(testNamespace))

			updateProfile(t, r, profileKey(), func(profile *clusterinventory.ClusterProfile) {
				profile.Status.AccessProviders = newBuiltinProviderClusterProfile(nil).Status.AccessProviders
			})

			_, err = r.Reconcile(context.Background(), profileRequest())
			require.NoError(t, err)
			recreatedSecret := getSecret(t, r, testNamespace)
			assert.True(t, metav1.IsControlledBy(recreatedSecret, clusterProfile))
			assert.Equal(t, testServer, string(recreatedSecret.Data[secretDataServerKey]))
		})

		t.Run("should preserve Secrets without the current ClusterProfile owner after access is revoked", func(t *testing.T) {
			controller := true
			testCases := []struct {
				name            string
				ownerReferences []metav1.OwnerReference
			}{
				{
					name: "ownerless Secret",
				},
				{
					name: "foreign-controlled Secret",
					ownerReferences: []metav1.OwnerReference{
						{
							APIVersion: "apps/v1",
							Kind:       "Deployment",
							Name:       "foreign-controller",
							UID:        "foreign-controller-uid",
							Controller: &controller,
						},
					},
				},
				{
					name: "Secret controlled by a previous ClusterProfile UID",
					ownerReferences: []metav1.OwnerReference{
						clusterProfileOwnerReference(testClusterName, "previous-cluster-profile-uid"),
					},
				},
			}

			for _, testCase := range testCases {
				t.Run(testCase.name, func(t *testing.T) {
					clusterProfile := newClusterProfile(nil)
					secret := newUnownedSecret(
						testNamespace,
						"preserved-secret-uid",
						map[string]string{managedByLabel: "external"},
						testCase.ownerReferences,
					)
					before := secret.DeepCopy()
					r := &ClusterProfileReconciler{
						Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
						Log:    logr.Discard(),
						Scheme: scheme,
					}

					_, err := r.Reconcile(context.Background(), profileRequest())
					require.NoError(t, err)

					assert.Equal(t, before, getSecret(t, r, testNamespace))
				})
			}
		})

		t.Run("should use delete preconditions and retry when revoked Secret deletion conflicts", func(t *testing.T) {
			clusterProfile := newClusterProfile(nil)
			secret := newControlledSecret(clusterProfile, "revoked-secret-uid")
			baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build()
			conflictClient := &deleteConflictClient{Client: baseClient}
			r := &ClusterProfileReconciler{
				Client: conflictClient,
				Log:    logr.Discard(),
				Scheme: scheme,
			}

			_, err := r.Reconcile(context.Background(), profileRequest())

			require.Error(t, err)
			assert.True(t, apierrors.IsConflict(err))
			assertDeletePreconditions(t, conflictClient, secret)
			assert.Equal(t, secret.UID, getSecret(t, r, testNamespace).UID)
		})

		t.Run("should continue using a deprecated credential provider when access providers are empty", func(t *testing.T) {
			providersFile := writeProvidersFile(t)
			clusterProfile := newCustomProviderClusterProfile(nil)
			clusterProfile.Status.CredentialProviders = clusterProfile.Status.AccessProviders
			clusterProfile.Status.AccessProviders = nil
			r := &ClusterProfileReconciler{
				Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:                        logr.Discard(),
				Scheme:                     scheme,
				ClusterProfileProviderFile: providersFile,
			}
			require.NoError(t, r.loadClusterProfileProviderFile())

			_, err := r.Reconcile(context.Background(), profileRequest())

			require.NoError(t, err)
			secret := getSecret(t, r, testNamespace)
			assert.Equal(t, testServer, string(secret.Data[secretDataServerKey]))
			assert.True(t, metav1.IsControlledBy(secret, clusterProfile))
		})

		t.Run("should create a secret per namespace for same-named ClusterProfiles", func(t *testing.T) {
			profileA := newBuiltinProviderClusterProfile(nil)
			profileA.Namespace = teamANamespace
			profileA.UID = "uid-team-a"
			profileB := newBuiltinProviderClusterProfile(nil)
			profileB.Namespace = teamBNamespace
			profileB.UID = "uid-team-b"
			profileB.Status.AccessProviders[0].Cluster.Server = "https://team-b.example.com"
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(profileA, profileB).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}

			for _, namespace := range []string{teamANamespace, teamBNamespace} {
				_, err := r.Reconcile(context.Background(), reconcile.Request{
					NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: namespace},
				})
				require.NoError(t, err)
			}

			secretA := getSecret(t, r, teamANamespace)
			assert.Equal(t, testClusterName, secretA.Labels[clusterProfileNameKey])
			assert.Equal(t, testServer, string(secretA.Data[secretDataServerKey]))
			assert.True(t, metav1.IsControlledBy(secretA, profileA))

			secretB := getSecret(t, r, teamBNamespace)
			assert.Equal(t, testClusterName, secretB.Labels[clusterProfileNameKey])
			assert.Equal(t, "https://team-b.example.com", string(secretB.Data[secretDataServerKey]))
			assert.True(t, metav1.IsControlledBy(secretB, profileB))
		})

		t.Run("should generate and prune a bounded Secret for a long ClusterProfile name", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(nil)
			clusterProfile.Name = strings.Repeat("a", 246)
			boundedSecretKey := clusterProfileSecretKey(clusterProfile)
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clusterProfile)}

			_, err := r.Reconcile(context.Background(), request)
			require.NoError(t, err)

			secret := &corev1.Secret{}
			require.NoError(t, r.Get(context.Background(), boundedSecretKey, secret))
			assert.True(t, metav1.IsControlledBy(secret, clusterProfile))
			// The label is bounded; the annotation and data keep the full name readable.
			assert.Len(t, secret.Labels[clusterProfileNameKey], content.LabelValueMaxLength)
			assert.Equal(t, clusterProfile.Name, secret.Annotations[clusterProfileNameKey])
			assert.Equal(t, clusterProfile.Name, string(secret.Data[secretDataNameKey]))

			// The delete path derives the same bounded name.
			updateProfile(t, r, request.NamespacedName, func(profile *clusterinventory.ClusterProfile) {
				profile.Status.AccessProviders = nil
			})
			_, err = r.Reconcile(context.Background(), request)
			require.NoError(t, err)
			requireNoSecret(t, r, boundedSecretKey)
		})
	})
}

func TestLoadClusterProfileProviderFile(t *testing.T) {
	t.Run("should not return an error if the provider file is not specified", func(t *testing.T) {
		r := &ClusterProfileReconciler{
			Log: logr.Discard(),
		}
		err := r.loadClusterProfileProviderFile()
		assert.NoError(t, err)
	})

	t.Run("should return an error if the provider file does not exist", func(t *testing.T) {
		r := &ClusterProfileReconciler{
			Log:                        logr.Discard(),
			ClusterProfileProviderFile: "non-existent-file",
		}
		err := r.loadClusterProfileProviderFile()
		assert.Error(t, err)
	})
}

func TestNewCommandClusterProfileProviderFileFlag(t *testing.T) {
	t.Setenv("ARGOCD_CLUSTERPROFILE_CONTROLLER_CLUSTERPROFILE_PROVIDER_FILE", "/tmp/access.json")

	command := NewCommand()
	providerFileFlag := command.Flags().Lookup("clusterprofile-provider-file")

	require.NotNil(t, providerFileFlag)
	assert.Equal(t, "/tmp/access.json", providerFileFlag.DefValue)
}

func TestNewCommandClusterProfileNamespacesAllNamespacesSentinel(t *testing.T) {
	t.Setenv("ARGOCD_CLUSTERPROFILE_CONTROLLER_NAMESPACES", "*")

	command := NewCommand()

	namespacesFlag := command.Flags().Lookup("cluster-profile-namespaces")
	require.NotNil(t, namespacesFlag)
	assert.Equal(t, "[*]", namespacesFlag.DefValue)
}

func TestBuildCacheOptions(t *testing.T) {
	t.Run("defaults ClusterProfiles and Secrets to the controller namespace", func(t *testing.T) {
		options := buildCacheOptions(argocdNamespace, nil)

		assertCacheNamespaces(t, options, &clusterinventory.ClusterProfile{}, []string{argocdNamespace})
		assertCacheNamespaces(t, options, &corev1.Secret{}, []string{argocdNamespace})
	})

	t.Run("defaults ClusterProfiles to the controller namespace when namespace entries are blank", func(t *testing.T) {
		options := buildCacheOptions(argocdNamespace, []string{"", " ", "\t"})

		assertCacheNamespaces(t, options, &clusterinventory.ClusterProfile{}, []string{argocdNamespace})
		assertCacheNamespaces(t, options, &corev1.Secret{}, []string{argocdNamespace})
	})

	t.Run("limits ClusterProfiles and Secrets to the same explicit namespaces", func(t *testing.T) {
		options := buildCacheOptions(
			argocdNamespace,
			[]string{teamANamespace, " team-b ", teamANamespace, ""},
		)

		assertCacheNamespaces(t, options, &clusterinventory.ClusterProfile{}, []string{teamANamespace, teamBNamespace})
		assertCacheNamespaces(t, options, &corev1.Secret{}, []string{teamANamespace, teamBNamespace})
	})

	// An empty (but non-nil) namespace map defers to the default cluster-wide cache.
	t.Run("watches ClusterProfiles and Secrets in all namespaces when the wildcard is requested", func(t *testing.T) {
		options := buildCacheOptions(argocdNamespace, []string{"*"})

		assertCacheNamespaces(t, options, &clusterinventory.ClusterProfile{}, []string{})
		assertCacheNamespaces(t, options, &corev1.Secret{}, []string{})
	})

	t.Run("the wildcard takes precedence over explicit namespaces", func(t *testing.T) {
		options := buildCacheOptions(argocdNamespace, []string{teamANamespace, "*"})

		assertCacheNamespaces(t, options, &clusterinventory.ClusterProfile{}, []string{})
		assertCacheNamespaces(t, options, &corev1.Secret{}, []string{})
	})
}

func assertCacheNamespaces(
	t *testing.T,
	options cache.Options,
	object client.Object,
	expectedNamespaces []string,
) {
	t.Helper()

	// ByObject is keyed by pointer identity, so the entry has to be found by type.
	objectType := reflect.TypeOf(object)
	for cachedObject, byObject := range options.ByObject {
		if reflect.TypeOf(cachedObject) == objectType {
			require.NotNil(t, byObject.Namespaces)
			assert.ElementsMatch(t, expectedNamespaces, slices.Collect(maps.Keys(byObject.Namespaces)))
			return
		}
	}
	t.Fatalf("cache options do not include object type %T", object)
}
