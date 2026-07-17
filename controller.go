package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterinventory "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/argoproj/argo-cd/v3/common"
	v1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const (
	// secretNameTemplate is the template used to generate the name of the Secret for a ClusterProfile.
	secretNameTemplate = "cluster-%s"
	// boundedSecretNamePrefix is disjoint from raw names produced by secretNameTemplate.
	boundedSecretNamePrefix = "clusterprofile-"
	// generatedMetadataHashLength is 128 bits of a SHA-256 digest, hex-encoded.
	generatedMetadataHashLength = 32
	// clusterProfileNameKey identifies the ClusterProfile that a Secret was created
	// from. The label value is bounded for the label value limit; the annotation
	// with the same key always carries the full name.
	clusterProfileNameKey = "argocd.argoproj.io/cluster-profile-name"
	secretDataNameKey     = "name"
	secretDataServerKey   = "server"
	secretDataConfigKey   = "config"
	// Fingerprint annotations stamped on successful renders; see handleOwnedSecretAfterRenderFailure.
	secretAccessProviderFingerprintAnnotation = "argocd.argoproj.io/cluster-profile-access-provider-fingerprint"
	secretPayloadFingerprintAnnotation        = "argocd.argoproj.io/cluster-profile-secret-payload-fingerprint"
	fingerprintPrefix                         = "v1:sha256:"
	// builtinCloudProviderAWS/Azure/GCP are the argocd-k8s-auth cloud providers accepted from an
	// "argo-cd-builtin-" access provider name.
	builtinCloudProviderAWS   = "aws"
	builtinCloudProviderAzure = "azure"
	builtinCloudProviderGCP   = "gcp"
)

type renderedSecret struct {
	data                      map[string][]byte
	labels                    map[string]string
	accessProviderFingerprint string
	payloadFingerprint        string
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
}

//+kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=clusterprofiles,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *ClusterProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	if !clusterProfile.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// If the ClusterProfile no longer advertises access, prune the Secret it owns.
	if len(clusterProfile.Status.AccessProviders) == 0 &&
		len(clusterProfile.Status.CredentialProviders) == 0 {
		if err := r.deleteOwnedSecret(ctx, &clusterProfile); err != nil {
			log.Error(err, "unable to remove secret after ClusterProfile access was revoked")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	rendered, err := r.renderSecret(&clusterProfile)
	if err != nil {
		if handleErr := r.handleOwnedSecretAfterRenderFailure(ctx, &clusterProfile); handleErr != nil {
			err = errors.Join(err, handleErr)
		}
		log.Error(err, "unable to render secret for ClusterProfile")
		return ctrl.Result{}, err
	}

	// Create or update the secret in the ClusterProfile's namespace.
	secretName := clusterProfileSecretName(&clusterProfile)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: clusterProfile.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		return r.mutateSecret(secret, &clusterProfile, rendered)
	})
	if err != nil {
		log.Error(err, "unable to create or update secret for ClusterProfile")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handleOwnedSecretAfterRenderFailure retains the last rendered Secret unless
// its fingerprints prove the payload is unchanged and its provider is no
// longer advertised. See docs/ARCHITECTURE.md for the full failure-state table.
func (r *ClusterProfileReconciler) handleOwnedSecretAfterRenderFailure(
	ctx context.Context,
	clusterProfile *clusterinventory.ClusterProfile,
) error {
	secret, err := r.ownedSecret(ctx, clusterProfile)
	if err != nil || secret == nil {
		return err
	}

	storedProvider := secret.Annotations[secretAccessProviderFingerprintAnnotation]
	storedPayload := secret.Annotations[secretPayloadFingerprintAnnotation]
	if !validFingerprint(storedProvider) || !validFingerprint(storedPayload) {
		return nil
	}
	actualPayload, err := fingerprintSecretPayload(secret.Labels, secret.Data)
	if err != nil {
		return nil
	}
	if storedPayload != actualPayload {
		return nil
	}

	providerStillAdvertised, err := accessProviderFingerprintExists(clusterProfile, storedProvider)
	if err != nil {
		return nil
	}
	if !providerStillAdvertised {
		if err := r.deleteSecretWithPreconditions(ctx, secret); err != nil {
			return err
		}
		r.Log.Info(
			"removed obsolete Secret after ClusterProfile access provider changed",
			"clusterprofile", client.ObjectKeyFromObject(clusterProfile),
			"secret", client.ObjectKeyFromObject(secret),
		)
		return nil
	}

	desiredLabels := generatedSecretLabels(clusterProfile)
	if maps.Equal(secret.Labels, desiredLabels) {
		return nil
	}
	before := secret.DeepCopy()
	secret.Labels = desiredLabels
	payloadFingerprint, err := fingerprintSecretPayload(secret.Labels, secret.Data)
	if err != nil {
		return nil
	}
	secret.Annotations[secretPayloadFingerprintAnnotation] = payloadFingerprint
	patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
	if err := r.Patch(ctx, secret, patch); err != nil {
		return fmt.Errorf(
			"failed to update Secret labels after render failure %s: %w",
			client.ObjectKeyFromObject(secret),
			err,
		)
	}
	r.Log.Info(
		"updated generated Secret labels while retaining last-known-good credentials",
		"clusterprofile", client.ObjectKeyFromObject(clusterProfile),
		"secret", client.ObjectKeyFromObject(secret),
	)
	return nil
}

func (r *ClusterProfileReconciler) deleteOwnedSecret(
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
	key := client.ObjectKey{
		Name:      clusterProfileSecretName(clusterProfile),
		Namespace: clusterProfile.Namespace,
	}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get owned Secret %s: %w", key, err)
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
		return fmt.Errorf("failed to delete Secret %s: %w", client.ObjectKeyFromObject(secret), err)
	}
	return nil
}

// validateSecretProvenance rejects persisted Secrets this ClusterProfile does not
// already control. SetControllerReference is not enough on its own: it matches on
// group, kind and name, so it would adopt an ownerless Secret or overwrite an
// ownerReference carrying a stale UID.
func validateSecretProvenance(
	secret *corev1.Secret,
	clusterProfile *clusterinventory.ClusterProfile,
) error {
	// CreateOrUpdate passes an unpersisted Secret when it is about to create one.
	if secret.UID == "" {
		return nil
	}
	if metav1.IsControlledBy(secret, clusterProfile) {
		return nil
	}

	return fmt.Errorf(
		"refusing to mutate Secret %s without provenance from ClusterProfile %s",
		client.ObjectKeyFromObject(secret),
		client.ObjectKeyFromObject(clusterProfile),
	)
}

// renderSecret builds the desired Secret payload without mutating persisted state.
func (r *ClusterProfileReconciler) renderSecret(
	clusterProfile *clusterinventory.ClusterProfile,
) (*renderedSecret, error) {
	// A supported built-in access provider takes precedence over custom providers.
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

	// Build the kubeconfig from a clone; the dependency mutates nested exec config.
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
	server := provider.Cluster.Server
	if strings.TrimSpace(server) == "" {
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
	data := map[string][]byte{
		secretDataNameKey:   []byte(clusterProfile.Name),
		secretDataServerKey: []byte(server),
		secretDataConfigKey: config,
	}
	labels := generatedSecretLabels(clusterProfile)
	accessProviderFingerprint, err := fingerprintAccessProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("failed to fingerprint access provider: %w", err)
	}
	payloadFingerprint, err := fingerprintSecretPayload(labels, data)
	if err != nil {
		return nil, fmt.Errorf("failed to fingerprint Secret payload: %w", err)
	}
	return &renderedSecret{
		data:                      data,
		labels:                    labels,
		accessProviderFingerprint: accessProviderFingerprint,
		payloadFingerprint:        payloadFingerprint,
	}, nil
}

func (r *ClusterProfileReconciler) mutateSecret(
	secret *corev1.Secret,
	clusterProfile *clusterinventory.ClusterProfile,
	rendered *renderedSecret,
) error {
	if err := validateSecretProvenance(secret, clusterProfile); err != nil {
		return err
	}

	// BlockOwnerDeletion is disabled because nothing waits on deletion ordering and setting it
	// would require clusterprofiles/finalizers update permission under the
	// OwnerReferencesPermissionEnforcement admission plugin.
	if err := controllerutil.SetControllerReference(
		clusterProfile, secret, r.Scheme, controllerutil.WithBlockOwnerDeletion(false),
	); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	secret.Labels = rendered.labels
	secret.Data = rendered.data
	secret.StringData = nil
	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string, 3)
	}
	secret.Annotations[clusterProfileNameKey] = clusterProfile.Name
	secret.Annotations[secretAccessProviderFingerprintAnnotation] = rendered.accessProviderFingerprint
	secret.Annotations[secretPayloadFingerprintAnnotation] = rendered.payloadFingerprint
	return nil
}

func generatedSecretLabels(clusterProfile *clusterinventory.ClusterProfile) map[string]string {
	labels := make(map[string]string, len(clusterProfile.Labels)+2)
	for key, value := range clusterProfile.Labels {
		labels[key] = value
	}
	labels[common.LabelKeySecretType] = common.LabelValueSecretTypeCluster
	labels[clusterProfileNameKey] = clusterProfileNameLabelValue(clusterProfile)
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

func accessProviderFingerprintExists(
	clusterProfile *clusterinventory.ClusterProfile,
	wanted string,
) (bool, error) {
	for _, provider := range effectiveAccessProviders(clusterProfile) {
		fingerprint, err := fingerprintAccessProvider(provider)
		if err != nil {
			return false, err
		}
		if fingerprint == wanted {
			return true, nil
		}
	}
	return false, nil
}

func fingerprintAccessProvider(provider *clusterinventory.AccessProvider) (string, error) {
	canonicalProvider := provider.DeepCopy()
	sort.SliceStable(canonicalProvider.Cluster.Extensions, func(i, j int) bool {
		return canonicalProvider.Cluster.Extensions[i].Name < canonicalProvider.Cluster.Extensions[j].Name
	})
	for i := range canonicalProvider.Cluster.Extensions {
		extension := &canonicalProvider.Cluster.Extensions[i].Extension
		raw, err := json.Marshal(extension)
		if err != nil {
			return "", err
		}
		if bytes.Equal(raw, []byte("null")) {
			continue
		}
		canonical, err := canonicalJSON(raw)
		if err != nil {
			return "", err
		}
		*extension = runtime.RawExtension{Raw: canonical}
	}
	return fingerprintJSON(canonicalProvider)
}

func fingerprintSecretPayload(labels map[string]string, data map[string][]byte) (string, error) {
	return fingerprintJSON(struct {
		Labels map[string]string `json:"labels"`
		Data   map[string][]byte `json:"data"`
	}{Labels: labels, Data: data})
}

func fingerprintJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return fingerprintPrefix + hex.EncodeToString(digest[:]), nil
}

func canonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("unexpected data after JSON value")
	}
	return json.Marshal(value)
}

func validFingerprint(value string) bool {
	if !strings.HasPrefix(value, fingerprintPrefix) {
		return false
	}
	hexDigest := strings.TrimPrefix(value, fingerprintPrefix)
	if len(hexDigest) != sha256.Size*2 || hexDigest != strings.ToLower(hexDigest) {
		return false
	}
	_, err := hex.DecodeString(hexDigest)
	return err == nil
}

func clusterProfileSecretName(clusterProfile *clusterinventory.ClusterProfile) string {
	rawName := fmt.Sprintf(secretNameTemplate, clusterProfile.Name)
	if len(rawName) <= content.DNS1123SubdomainMaxLength {
		return rawName
	}

	return boundedMetadataValue(
		boundedSecretNamePrefix+clusterProfile.Name,
		clusterProfile.Name,
		content.DNS1123SubdomainMaxLength,
		"-",
	)
}

func clusterProfileNameLabelValue(clusterProfile *clusterinventory.ClusterProfile) string {
	if len(clusterProfile.Name) <= content.LabelValueMaxLength {
		return clusterProfile.Name
	}

	// Kubernetes names cannot contain underscores, so the internal marker makes
	// the bounded representation disjoint from every raw name.
	return boundedMetadataValue(
		clusterProfile.Name,
		clusterProfile.Name,
		content.LabelValueMaxLength,
		"_",
	)
}

// boundedMetadataValue retains a readable prefix and 128 bits of digest. The
// representation is collision-resistant, while provenance checks remain the
// authoritative protection against overwriting an existing Secret.
func boundedMetadataValue(value, hashInput string, maxLength int, separator string) string {
	digest := sha256.Sum256([]byte(hashInput))
	hash := hex.EncodeToString(digest[:])[:generatedMetadataHashLength]
	prefixLength := maxLength - len(hash) - len(separator)
	prefix := strings.TrimRight(value[:prefixLength], "-_.")
	return prefix + separator + hash
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
	// Controller sources acquire informers lazily when they start. Register
	// them now so the manager's cache-sync gate cannot complete against an
	// empty informer set before the controller starts.
	for _, obj := range []client.Object{&clusterinventory.ClusterProfile{}, &corev1.Secret{}} {
		if _, err := mgr.GetCache().GetInformer(
			context.Background(), obj, cache.BlockUntilSynced(false),
		); err != nil {
			return fmt.Errorf("failed to register informer for %T: %w", obj, err)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterinventory.ClusterProfile{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
