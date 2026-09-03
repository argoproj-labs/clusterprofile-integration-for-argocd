package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	clusterinventory "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/argoproj/argo-cd/v3/common"
	v1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const (
	// secretNameTemplate is the template used to generate the name of the Secret for a ClusterProfile.
	secretNameTemplate = "cluster-%s"
	// boundedSecretNamePrefix names Secrets whose profile name exceeds the length limit, and is
	// disjoint from raw names produced by secretNameTemplate.
	boundedSecretNamePrefix = "clusterprofile-"
	// generatedMetadataHashBytes is the number of digest bytes retained by boundedMetadataValue.
	generatedMetadataHashBytes = 16
	// clusterProfileNameKey identifies the ClusterProfile that a Secret was created from.
	clusterProfileNameKey = "argocd.argoproj.io/cluster-profile-name"
	secretDataNameKey     = "name"
	secretDataServerKey   = "server"
	secretDataConfigKey   = "config"
	// secretAccessProviderAnnotation is the annotation recording the access provider that rendered the Secret.
	secretAccessProviderAnnotation = "argocd.argoproj.io/cluster-profile-access-provider"
	// secretPayloadFingerprintAnnotation is the annotation recording a digest of the payload this controller wrote.
	secretPayloadFingerprintAnnotation = "argocd.argoproj.io/cluster-profile-secret-payload-fingerprint"
	// builtinCloudProviderAWS/Azure/GCP are the argocd-k8s-auth cloud providers accepted from an
	// "argo-cd-builtin-" access provider name.
	builtinCloudProviderAWS   = "aws"
	builtinCloudProviderAzure = "azure"
	builtinCloudProviderGCP   = "gcp"
	// Synthetic requests use an invalid object-name prefix to avoid ClusterProfile name collisions.
	inventoryMemberRequestPrefix = "inventory-member-id:"
	// TODO: Remove this local constant and use clusterinventory.LabelInventoryMemberIDKey after
	// upgrading to a release that includes kubernetes-sigs/cluster-inventory-api#83.
	inventoryMemberIDLabel = "multicluster.x-k8s.io/inventory-member-id"

	eventReasonDuplicateInventoryMemberID = "DuplicateInventoryMemberID"
	eventReasonAmbiguousInventoryMemberID = "AmbiguousInventoryMemberID"
	eventReasonInvalidInventoryMemberID   = "InvalidInventoryMemberID"
	eventActionResolveInventoryMember     = "ResolveInventoryMember"
)

type renderedSecret struct {
	data     map[string][]byte
	provider string
}

type inventoryMemberResolution string

const (
	inventoryMemberResolutionUnique    inventoryMemberResolution = "unique"
	inventoryMemberResolutionDuplicate inventoryMemberResolution = "duplicate"
	inventoryMemberResolutionAmbiguous inventoryMemberResolution = "ambiguous"
)

// keyedMutex serializes work for the same key while allowing different keys to proceed concurrently.
// Entries are retained only while a caller holds or waits for the corresponding lock.
type keyedMutex[K comparable] struct {
	mu      sync.Mutex
	entries map[K]*keyedMutexEntry
}

type keyedMutexEntry struct {
	mu    sync.Mutex
	users int
}

func (m *keyedMutex[K]) lock(key K) func() {
	m.mu.Lock()
	if m.entries == nil {
		m.entries = make(map[K]*keyedMutexEntry)
	}
	entry := m.entries[key]
	if entry == nil {
		entry = &keyedMutexEntry{}
		m.entries[key] = entry
	}
	entry.users++
	m.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()

		m.mu.Lock()
		defer m.mu.Unlock()
		entry.users--
		if entry.users == 0 {
			delete(m.entries, key)
		}
	}
}

// ClusterProfileReconciler reconciles a ClusterProfile object with a corresponding Secret
type ClusterProfileReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
	// ClusterProfileProviderFile is the path to the file containing the cluster profile provider configuration.
	ClusterProfileProviderFile string
	// AccessProviders is the set of access providers used to build the kubeconfig for a ClusterProfile.
	AccessProviders *access.Config
	recorder        events.EventRecorder
	memberLocks     keyedMutex[client.ObjectKey]
}

//+kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=clusterprofiles,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *ClusterProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if memberID, ok := strings.CutPrefix(req.Name, inventoryMemberRequestPrefix); ok {
		return ctrl.Result{}, r.reconcileInventoryMember(ctx, req.Namespace, memberID)
	}

	log := r.Log.WithValues("clusterprofile", req.NamespacedName)

	// Fetch Cluster Profile
	var clusterProfile clusterinventory.ClusterProfile
	if err := r.Get(ctx, req.NamespacedName, &clusterProfile); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch ClusterProfile")
		return ctrl.Result{}, err
	}

	// Direct requests for labeled profiles must pass through member selection.
	memberID, set := inventoryMemberID(&clusterProfile)
	if memberID != "" {
		return ctrl.Result{}, r.reconcileInventoryMember(ctx, clusterProfile.Namespace, memberID)
	}
	if set {
		r.warnInventoryMembers(
			[]*clusterinventory.ClusterProfile{&clusterProfile},
			eventReasonInvalidInventoryMemberID,
			fmt.Sprintf(
				"label %q must be non-empty when set; treating it as absent and not deduplicating this ClusterProfile",
				inventoryMemberIDLabel,
			),
		)
	}

	return ctrl.Result{}, r.reconcileClusterProfile(ctx, &clusterProfile)
}

// reconcileInventoryMember applies inventory-member selection within a namespace.
func (r *ClusterProfileReconciler) reconcileInventoryMember(
	ctx context.Context,
	namespace string,
	memberID string,
) error {
	unlock := r.memberLocks.lock(client.ObjectKey{Namespace: namespace, Name: memberID})
	defer unlock()

	profiles, err := r.listActiveInventoryMemberProfiles(ctx, namespace, memberID)
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		return nil
	}

	winner, resolution := resolveInventoryMember(profiles)
	switch resolution {
	case inventoryMemberResolutionAmbiguous:
		r.warnAmbiguousInventoryMember(profiles, namespace, memberID)
		return nil
	case inventoryMemberResolutionDuplicate:
		r.warnDuplicateInventoryMember(profiles, winner, namespace, memberID)
	}

	return r.reconcileClusterProfile(ctx, winner)
}

func (r *ClusterProfileReconciler) listActiveInventoryMemberProfiles(
	ctx context.Context,
	namespace string,
	memberID string,
) ([]*clusterinventory.ClusterProfile, error) {
	var clusterProfiles clusterinventory.ClusterProfileList
	if err := r.List(
		ctx,
		&clusterProfiles,
		client.InNamespace(namespace),
		client.MatchingLabels{inventoryMemberIDLabel: memberID},
	); err != nil {
		return nil, fmt.Errorf("failed to list ClusterProfiles for inventory member %q: %w", memberID, err)
	}

	profiles := make([]*clusterinventory.ClusterProfile, 0, len(clusterProfiles.Items))
	for i := range clusterProfiles.Items {
		profile := &clusterProfiles.Items[i]
		if profile.DeletionTimestamp.IsZero() {
			profiles = append(profiles, profile)
		}
	}
	return profiles, nil
}

// resolveInventoryMember sorts profiles for stable output; names do not break creationTimestamp ties.
func resolveInventoryMember(
	profiles []*clusterinventory.ClusterProfile,
) (*clusterinventory.ClusterProfile, inventoryMemberResolution) {
	if len(profiles) == 0 {
		return nil, ""
	}
	slices.SortFunc(profiles, func(a, b *clusterinventory.ClusterProfile) int {
		return cmp.Or(
			a.CreationTimestamp.Compare(b.CreationTimestamp.Time),
			strings.Compare(a.Name, b.Name),
		)
	})

	if len(profiles) == 1 {
		return profiles[0], inventoryMemberResolutionUnique
	}
	if profiles[1].CreationTimestamp.Equal(&profiles[0].CreationTimestamp) {
		return nil, inventoryMemberResolutionAmbiguous
	}
	return profiles[0], inventoryMemberResolutionDuplicate
}

func (r *ClusterProfileReconciler) warnAmbiguousInventoryMember(
	profiles []*clusterinventory.ClusterProfile,
	namespace string,
	memberID string,
) {
	r.warnInventoryMembers(profiles, eventReasonAmbiguousInventoryMemberID, fmt.Sprintf(
		"no ClusterProfile is selected for member ID %q in inventory namespace %q because multiple ClusterProfiles "+
			"share the oldest creationTimestamp %s",
		memberID, namespace, profiles[0].CreationTimestamp.UTC().Format(time.RFC3339),
	))
}

func (r *ClusterProfileReconciler) warnDuplicateInventoryMember(
	profiles []*clusterinventory.ClusterProfile,
	winner *clusterinventory.ClusterProfile,
	namespace string,
	memberID string,
) {
	r.warnInventoryMembers(profiles, eventReasonDuplicateInventoryMemberID, fmt.Sprintf(
		"%d ClusterProfiles in inventory namespace %q claim member ID %q; oldest ClusterProfile %q is selected",
		len(profiles), namespace, memberID, winner.Name,
	))
}

// warnInventoryMembers logs one warning and records it as a Warning Event on every listed ClusterProfile.
func (r *ClusterProfileReconciler) warnInventoryMembers(
	profiles []*clusterinventory.ClusterProfile,
	reason string,
	message string,
) {
	names := make([]string, len(profiles))
	for i, profile := range profiles {
		names[i] = profile.Name
	}
	memberID, _ := inventoryMemberID(profiles[0])
	r.Log.Info(
		message,
		"warning", true,
		"reason", reason,
		"namespace", profiles[0].Namespace,
		"inventoryMemberID", memberID,
		"clusterProfiles", names,
	)
	for _, profile := range profiles {
		r.recorder.Eventf(
			profile,
			nil,
			corev1.EventTypeWarning,
			reason,
			eventActionResolveInventoryMember,
			"%s",
			message,
		)
	}
}

func (r *ClusterProfileReconciler) reconcileClusterProfile(
	ctx context.Context,
	clusterProfile *clusterinventory.ClusterProfile,
) error {
	log := r.Log.WithValues("clusterprofile", client.ObjectKeyFromObject(clusterProfile))

	// Garbage collection removes the owned Secret, so deletion needs no work here.
	if !clusterProfile.DeletionTimestamp.IsZero() {
		return nil
	}

	// If the ClusterProfile no longer advertises access, prune the Secret it owns.
	if len(clusterProfile.Status.CredentialProviders)+len(clusterProfile.Status.AccessProviders) == 0 {
		if err := r.pruneSecret(ctx, clusterProfile); err != nil {
			log.Error(err, "unable to remove secret after ClusterProfile access was revoked")
			return err
		}
		return nil
	}

	rendered, err := r.renderSecret(clusterProfile)
	if err != nil {
		if handleErr := r.handleOwnedSecretAfterRenderFailure(ctx, clusterProfile); handleErr != nil {
			err = errors.Join(err, handleErr)
		}
		log.Error(err, "unable to render secret for ClusterProfile")
		return err
	}

	// Create or update the secret in the ClusterProfile's namespace.
	key := clusterProfileSecretKey(clusterProfile)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		return r.mutateSecret(secret, clusterProfile, rendered)
	})
	if err != nil {
		log.Error(err, "unable to create or update secret for ClusterProfile")
		return err
	}

	return nil
}

// handleOwnedSecretAfterRenderFailure prunes the generated Secret once the access provider that
// rendered it is withdrawn, and otherwise retains its last-known-good credentials.
func (r *ClusterProfileReconciler) handleOwnedSecretAfterRenderFailure(
	ctx context.Context,
	clusterProfile *clusterinventory.ClusterProfile,
) error {
	secret, err := r.ownedSecret(ctx, clusterProfile)
	if err != nil || secret == nil {
		return err
	}
	storedProvider := secret.Annotations[secretAccessProviderAnnotation]
	storedPayload := secret.Annotations[secretPayloadFingerprintAnnotation]
	if storedProvider == "" || storedPayload != fingerprintSecretData(secret.Data) {
		return nil
	}

	log := r.Log.WithValues(
		"clusterprofile", client.ObjectKeyFromObject(clusterProfile),
		"secret", client.ObjectKeyFromObject(secret),
	)

	if _, advertised := effectiveAccessProviders(clusterProfile)[storedProvider]; !advertised {
		if err := r.deleteSecretWithPreconditions(ctx, secret); err != nil {
			return err
		}
		log.Info("removed obsolete Secret after ClusterProfile access provider changed")
		return nil
	}

	desiredLabels := generatedSecretLabels(clusterProfile)
	if maps.Equal(secret.Labels, desiredLabels) {
		return nil
	}
	before := secret.DeepCopy()
	secret.Labels = desiredLabels
	patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
	if err := r.Patch(ctx, secret, patch); err != nil {
		return err
	}
	log.Info("updated generated Secret labels while retaining last-known-good credentials")
	return nil
}

// pruneSecret deletes the Secret associated with a ClusterProfile, if this
// ClusterProfile still controls it.
func (r *ClusterProfileReconciler) pruneSecret(
	ctx context.Context,
	clusterProfile *clusterinventory.ClusterProfile,
) error {
	secret, err := r.ownedSecret(ctx, clusterProfile)
	if err != nil || secret == nil {
		return err
	}
	return r.deleteSecretWithPreconditions(ctx, secret)
}

// ownedSecret fetches the generated Secret for a ClusterProfile, returning nil
// when it does not exist or is not controlled by this exact ClusterProfile.
func (r *ClusterProfileReconciler) ownedSecret(
	ctx context.Context,
	clusterProfile *clusterinventory.ClusterProfile,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, clusterProfileSecretKey(clusterProfile), secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if !metav1.IsControlledBy(secret, clusterProfile) {
		return nil, nil
	}
	return secret, nil
}

// deleteSecretWithPreconditions deletes the Secret only while its UID and
// resourceVersion are unchanged, so a concurrent update is never discarded.
func (r *ClusterProfileReconciler) deleteSecretWithPreconditions(
	ctx context.Context,
	secret *corev1.Secret,
) error {
	uid := secret.UID
	resourceVersion := secret.ResourceVersion
	preconditions := client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}
	if err := r.Delete(ctx, secret, preconditions); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// renderSecret builds the desired Secret payload without mutating persisted state.
func (r *ClusterProfileReconciler) renderSecret(
	clusterProfile *clusterinventory.ClusterProfile,
) (*renderedSecret, error) {
	// Check for supported cloud provider. For example, a Cluster Profile with an access provider named
	// "argo-cd-builtin-gcp" will authenticate with cmd/argocd-k8s-auth/commands/gcp.go directly, without
	// requiring an access providers file.
	for i := range clusterProfile.Status.AccessProviders {
		provider := &clusterProfile.Status.AccessProviders[i]
		if !strings.HasPrefix(provider.Name, "argo-cd-builtin-") {
			continue
		}
		cloudProvider := strings.TrimPrefix(provider.Name, "argo-cd-builtin-")
		switch cloudProvider {
		case builtinCloudProviderAWS, builtinCloudProviderAzure, builtinCloudProviderGCP:
		default:
			return nil, fmt.Errorf(
				"unsupported built-in access provider %q for ClusterProfile %q",
				provider.Name,
				clusterProfile.Name,
			)
		}
		apiConfig := clusterConfigFromAccessProvider(provider)
		apiConfig.ExecProviderConfig = &v1alpha1.ExecProviderConfig{
			Command:    "argocd-k8s-auth",
			Args:       []string{cloudProvider},
			APIVersion: "client.authentication.k8s.io/v1beta1",
		}
		return newRenderedSecret(clusterProfile, provider, apiConfig)
	}

	if r.AccessProviders == nil {
		return nil, fmt.Errorf(
			"ClusterProfileReconciler AccessProviders not initialized. Required for custom config for ClusterProfile: %v",
			clusterProfile.Name,
		)
	}

	selectedProvider := selectCustomAccessProvider(clusterProfile, r.AccessProviders)
	if selectedProvider == nil {
		return nil, fmt.Errorf("no matching access provider found for ClusterProfile %q", clusterProfile.Name)
	}

	// If using custom access providers, build the kubeconfig.
	accessProviders := cloneAccessConfig(r.AccessProviders)
	config, err := accessProviders.BuildConfigFromCP(clusterProfile)
	if err != nil {
		return nil, fmt.Errorf("failed to build config: %w", err)
	}

	// Auth material comes from BuildConfigFromCP; connection fields from the AccessProvider.
	apiConfig := clusterConfigFromAccessProvider(selectedProvider)
	apiConfig.BearerToken = config.BearerToken
	apiConfig.CertData = config.CertData
	apiConfig.KeyData = config.KeyData

	// If there is an exec provider, add it to the config.
	if config.ExecProvider != nil {
		args := make([]string, len(config.ExecProvider.Args))
		for i, arg := range config.ExecProvider.Args {
			replaced := strings.ReplaceAll(arg, "{{ .ClusterProfileName }}", clusterProfile.Name)
			replaced = strings.ReplaceAll(
				replaced,
				"{{ .ClusterProfileServer }}",
				selectedProvider.Cluster.Server,
			)
			args[i] = replaced
		}
		apiConfig.ExecProviderConfig = &v1alpha1.ExecProviderConfig{
			Command:            config.ExecProvider.Command,
			Args:               args,
			APIVersion:         config.ExecProvider.APIVersion,
			ProvideClusterInfo: config.ExecProvider.ProvideClusterInfo,
		}
		if len(config.ExecProvider.Env) > 0 {
			apiConfig.ExecProviderConfig.Env = make(map[string]string)
			for _, env := range config.ExecProvider.Env {
				apiConfig.ExecProviderConfig.Env[env.Name] = env.Value
			}
		}
		// Preserve the exec provider's Config (e.g., clusterName from ClusterProfile extensions).
		// This data originates from the ClusterProfile's cluster.extensions field with the reserved key
		// "client.authentication.k8s.io/exec", as defined by the Kubernetes client authentication API.
		// Reference: https://kubernetes.io/docs/reference/config-api/kubeconfig.v1/#ExecConfig
		if config.ExecProvider.Config != nil {
			if configData, err := json.Marshal(config.ExecProvider.Config); err == nil {
				apiConfig.ExecProviderConfig.Config = &runtime.RawExtension{Raw: configData}
			}
		}
	}

	return newRenderedSecret(clusterProfile, selectedProvider, apiConfig)
}

func clusterConfigFromAccessProvider(provider *clusterinventory.AccessProvider) v1alpha1.ClusterConfig {
	return v1alpha1.ClusterConfig{
		TLSClientConfig: v1alpha1.TLSClientConfig{
			Insecure:   provider.Cluster.InsecureSkipTLSVerify,
			ServerName: provider.Cluster.TLSServerName,
			CAData:     provider.Cluster.CertificateAuthorityData,
		},
		DisableCompression: provider.Cluster.DisableCompression,
		ProxyUrl:           provider.Cluster.ProxyURL,
	}
}

func newRenderedSecret(
	clusterProfile *clusterinventory.ClusterProfile,
	provider *clusterinventory.AccessProvider,
	apiConfig v1alpha1.ClusterConfig,
) (*renderedSecret, error) {
	if strings.TrimSpace(provider.Cluster.Server) == "" {
		return nil, fmt.Errorf(
			"access provider %q for ClusterProfile %q has an empty cluster server",
			provider.Name,
			clusterProfile.Name,
		)
	}

	config, err := json.Marshal(apiConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config: %w", err)
	}
	return &renderedSecret{
		data: map[string][]byte{
			secretDataNameKey:   []byte(clusterProfile.Name),
			secretDataServerKey: []byte(provider.Cluster.Server),
			secretDataConfigKey: config,
		},
		provider: provider.Name,
	}, nil
}

// mutateSecret populates the secret with data from the ClusterProfile.
func (r *ClusterProfileReconciler) mutateSecret(
	secret *corev1.Secret,
	clusterProfile *clusterinventory.ClusterProfile,
	rendered *renderedSecret,
) error {
	// SetControllerReference matches on group, kind and name only, so on its own it would adopt
	// a persisted ownerless Secret or overwrite an ownerReference carrying a stale UID.
	if secret.UID != "" && !metav1.IsControlledBy(secret, clusterProfile) {
		return fmt.Errorf(
			"refusing to mutate Secret %s without provenance from ClusterProfile %s",
			client.ObjectKeyFromObject(secret),
			client.ObjectKeyFromObject(clusterProfile),
		)
	}

	// BlockOwnerDeletion is disabled because nothing waits on deletion ordering and setting it
	// would require clusterprofiles/finalizers update permission under the
	// OwnerReferencesPermissionEnforcement admission plugin.
	if err := controllerutil.SetControllerReference(
		clusterProfile, secret, r.Scheme, controllerutil.WithBlockOwnerDeletion(false),
	); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	secret.Labels = generatedSecretLabels(clusterProfile)
	secret.Data = rendered.data
	metav1.SetMetaDataAnnotation(&secret.ObjectMeta, clusterProfileNameKey, clusterProfile.Name)
	metav1.SetMetaDataAnnotation(&secret.ObjectMeta, secretAccessProviderAnnotation, rendered.provider)
	metav1.SetMetaDataAnnotation(
		&secret.ObjectMeta, secretPayloadFingerprintAnnotation, fingerprintSecretData(rendered.data),
	)
	return nil
}

// generatedSecretLabels identifies the secret as a cluster secret and links it to the ClusterProfile.
func generatedSecretLabels(clusterProfile *clusterinventory.ClusterProfile) map[string]string {
	labels := make(map[string]string, len(clusterProfile.Labels)+2)
	for key, value := range clusterProfile.Labels {
		labels[key] = value
	}
	labels[common.LabelKeySecretType] = common.LabelValueSecretTypeCluster
	labels[clusterProfileNameKey] = clusterProfileNameLabelValue(clusterProfile.Name)
	return labels
}

func selectCustomAccessProvider(
	clusterProfile *clusterinventory.ClusterProfile,
	config *access.Config,
) *clusterinventory.AccessProvider {
	effective := effectiveAccessProviders(clusterProfile)
	for _, configured := range config.Providers {
		if provider, ok := effective[configured.Name]; ok {
			return provider
		}
	}
	return nil
}

func effectiveAccessProviders(
	clusterProfile *clusterinventory.ClusterProfile,
) map[string]*clusterinventory.AccessProvider {
	providers := make(map[string]*clusterinventory.AccessProvider,
		len(clusterProfile.Status.CredentialProviders)+len(clusterProfile.Status.AccessProviders))
	for i := range clusterProfile.Status.CredentialProviders {
		provider := &clusterProfile.Status.CredentialProviders[i]
		providers[provider.Name] = provider
	}
	for i := range clusterProfile.Status.AccessProviders {
		provider := &clusterProfile.Status.AccessProviders[i]
		providers[provider.Name] = provider
	}
	return providers
}

// fingerprintSecretData digests the payload this controller wrote, so a later render
// failure can tell an untouched Secret from one an external writer has changed.
func fingerprintSecretData(data map[string][]byte) string {
	// json.Marshal cannot fail for map[string][]byte.
	encoded, _ := json.Marshal(data)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// clusterProfileSecretKey addresses the generated Secret in the ClusterProfile's own namespace.
func clusterProfileSecretKey(clusterProfile *clusterinventory.ClusterProfile) client.ObjectKey {
	return client.ObjectKey{
		Name:      clusterProfileSecretName(clusterProfile.Name),
		Namespace: clusterProfile.Namespace,
	}
}

func clusterProfileSecretName(profileName string) string {
	rawName := fmt.Sprintf(secretNameTemplate, profileName)
	if len(rawName) <= content.DNS1123SubdomainMaxLength {
		return rawName
	}

	return boundedMetadataValue(
		boundedSecretNamePrefix,
		profileName,
		content.DNS1123SubdomainMaxLength,
		"-",
	)
}

func clusterProfileNameLabelValue(profileName string) string {
	if len(profileName) <= content.LabelValueMaxLength {
		return profileName
	}

	// Kubernetes names cannot contain underscores, so the separator keeps the
	// bounded value disjoint from every raw name.
	return boundedMetadataValue(
		"",
		profileName,
		content.LabelValueMaxLength,
		"_",
	)
}

// boundedMetadataValue bounds prefix+name to maxLength, retaining a readable prefix and 128 bits
// of digest. Only call it for a name that already exceeds maxLength.
func boundedMetadataValue(prefix, name string, maxLength int, separator string) string {
	digest := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(digest[:generatedMetadataHashBytes])
	value := prefix + name
	return strings.TrimRight(value[:maxLength-len(hash)-len(separator)], "-_.") + separator + hash
}

func cloneAccessConfig(config *access.Config) *access.Config {
	if config == nil {
		return nil
	}
	clone := &access.Config{}
	if len(config.Providers) == 0 {
		return clone
	}
	clone.Providers = make([]access.Provider, len(config.Providers))
	for i, provider := range config.Providers {
		clone.Providers[i] = provider
		if provider.ExecConfig != nil {
			clone.Providers[i].ExecConfig = provider.ExecConfig.DeepCopy()
		}
	}
	return clone
}

// inventoryMemberID returns the label value and whether the label is present.
func inventoryMemberID(object client.Object) (string, bool) {
	memberID, set := object.GetLabels()[inventoryMemberIDLabel]
	return memberID, set
}

// inventoryMemberRequest groups labeled profiles and maps unlabeled profiles to their object key.
func inventoryMemberRequest(_ context.Context, clusterProfile client.Object) []reconcile.Request {
	key := client.ObjectKeyFromObject(clusterProfile)
	if memberID, _ := inventoryMemberID(clusterProfile); memberID != "" {
		key.Name = inventoryMemberRequestPrefix + memberID
	}
	return []reconcile.Request{{NamespacedName: key}}
}

// ownedSecretRequests maps a Secret to its owner's member group, falling back to the owner key
// while the ClusterProfile is absent from the cache.
func (r *ClusterProfileReconciler) ownedSecretRequests(ctx context.Context, secret client.Object) []reconcile.Request {
	owner := metav1.GetControllerOf(secret)
	if owner == nil || owner.Kind != "ClusterProfile" {
		return nil
	}
	if ownerGV, err := schema.ParseGroupVersion(owner.APIVersion); err != nil ||
		ownerGV.Group != clusterinventory.GroupVersion.Group {
		return nil
	}
	key := client.ObjectKey{Namespace: secret.GetNamespace(), Name: owner.Name}
	var clusterProfile clusterinventory.ClusterProfile
	if err := r.Get(ctx, key, &clusterProfile); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Log.Error(err, "unable to map owned Secret to its inventory member",
				"secret", client.ObjectKeyFromObject(secret), "clusterProfile", key)
		}
		return []reconcile.Request{{NamespacedName: key}}
	}
	return inventoryMemberRequest(ctx, &clusterProfile)
}

func (r *ClusterProfileReconciler) loadClusterProfileProviderFile() error {
	// TODO: do we need to reload periodically? (unlikely)
	if r.ClusterProfileProviderFile == "" {
		r.Log.Info("no cluster profile provider file specified, skipping")
		return nil
	}
	providers, err := access.NewFromFile(r.ClusterProfileProviderFile)
	if err != nil {
		return fmt.Errorf("failed to get providers from file: %w", err)
	}
	r.AccessProviders = providers
	return nil
}

func (r *ClusterProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// If using a supported cloud provider, this step will be skipped as no file is needed.
	if err := r.loadClusterProfileProviderFile(); err != nil {
		return err
	}
	r.recorder = mgr.GetEventRecorder(cliName)
	return ctrl.NewControllerManagedBy(mgr).
		Named("clusterprofile").
		Watches(&clusterinventory.ClusterProfile{}, handler.EnqueueRequestsFromMapFunc(inventoryMemberRequest)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.ownedSecretRequests)).
		Complete(r)
}
