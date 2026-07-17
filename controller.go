package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterinventory "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/argoproj/argo-cd/v3/common"
	v1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const (
	// secretNameTemplate is the template used to generate the name of the Secret for a ClusterProfile.
	secretNameTemplate = "cluster-%s"
	// clusterProfileOriginLabel is the label used to identify the ClusterProfile that a Secret was created from.
	clusterProfileOriginLabel = "argocd.argoproj.io/cluster-profile-origin"
	secretDataNameKey         = "name"
	secretDataServerKey       = "server"
	secretDataConfigKey       = "config"
)

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
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch

func (r *ClusterProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("clusterprofile", req.NamespacedName)

	// Fetch Cluster Profile
	var clusterProfile clusterinventory.ClusterProfile
	if err := r.Get(ctx, req.NamespacedName, &clusterProfile); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch ClusterProfile")
		return ctrl.Result{}, err
	}

	if !clusterProfile.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Create or update the secret in the ClusterProfile's namespace.
	secretName := fmt.Sprintf(secretNameTemplate, clusterProfile.Name)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: clusterProfile.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		return r.mutateSecret(secret, &clusterProfile)
	})
	if err != nil {
		log.Error(err, "unable to create or update secret for ClusterProfile")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// mutateSecret populates the secret with data from the ClusterProfile.
func (r *ClusterProfileReconciler) mutateSecret(
	secret *corev1.Secret,
	clusterProfile *clusterinventory.ClusterProfile,
) error {
	// BlockOwnerDeletion is disabled because nothing waits on deletion ordering and setting it
	// would require clusterprofiles/finalizers update permission under the
	// OwnerReferencesPermissionEnforcement admission plugin.
	if err := controllerutil.SetControllerReference(
		clusterProfile, secret, r.Scheme, controllerutil.WithBlockOwnerDeletion(false),
	); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Set labels on the secret to identify it as a cluster secret and link it to the ClusterProfile.
	labels := make(map[string]string, len(clusterProfile.Labels)+2)
	for key, value := range clusterProfile.Labels {
		labels[key] = value
	}
	labels[common.LabelKeySecretType] = common.LabelValueSecretTypeCluster
	labels[clusterProfileOriginLabel] = fmt.Sprintf("%s-%s", clusterProfile.Namespace, clusterProfile.Name)
	secret.Labels = labels

	// Check for supported cloud provider. For example, a Cluster Profile with an access provider named
	// "argo-cd-builtin-gcp" will authenticate with cmd/argocd-k8s-auth/commands/gcp.go directly, without
	// requiring an access providers file.
	for _, provider := range clusterProfile.Status.AccessProviders {
		if !strings.HasPrefix(provider.Name, "argo-cd-builtin-") {
			continue
		}
		cloudProvider := strings.TrimPrefix(provider.Name, "argo-cd-builtin-")
		apiConfig := v1alpha1.ClusterConfig{
			ExecProviderConfig: &v1alpha1.ExecProviderConfig{
				Command:    "argocd-k8s-auth",
				Args:       []string{cloudProvider},
				APIVersion: "client.authentication.k8s.io/v1beta1",
			},
		}
		configBytes, err := json.Marshal(apiConfig)
		if err != nil {
			return fmt.Errorf("failed to marshal config: %w", err)
		}
		secret.StringData = map[string]string{
			secretDataNameKey:   clusterProfile.Name,
			secretDataServerKey: provider.Cluster.Server,
			secretDataConfigKey: string(configBytes),
		}
		return nil
	}
	if r.AccessProviders == nil {
		return fmt.Errorf(
			"ClusterProfileReconciler AccessProviders not initialized. Required for custom config for ClusterProfile: %v",
			clusterProfile.Name,
		)
	}

	// If using custom access providers, build the kubeconfig.
	accessProviders := cloneAccessConfig(r.AccessProviders)
	config, err := accessProviders.BuildConfigFromCP(clusterProfile)
	if err != nil {
		return fmt.Errorf("failed to build config: %w", err)
	}

	apiConfig := v1alpha1.ClusterConfig{
		BearerToken: config.BearerToken,
		TLSClientConfig: v1alpha1.TLSClientConfig{
			Insecure:   config.Insecure,
			ServerName: config.ServerName,
			CertData:   config.CertData,
			KeyData:    config.KeyData,
			CAData:     config.CAData,
		},
		DisableCompression: config.DisableCompression,
	}

	// If there is an exec provider, add it to the config.
	if config.ExecProvider != nil {
		args := make([]string, len(config.ExecProvider.Args))
		for i, arg := range config.ExecProvider.Args {
			replaced := strings.ReplaceAll(arg, "{{ .ClusterProfileName }}", clusterProfile.Name)
			replaced = strings.ReplaceAll(
				replaced,
				"{{ .ClusterProfileServer }}",
				config.Host,
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

	configBytes, err := json.Marshal(apiConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	secret.StringData = map[string]string{
		secretDataNameKey:   clusterProfile.Name,
		secretDataServerKey: config.Host,
		secretDataConfigKey: string(configBytes),
	}

	return nil
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterinventory.ClusterProfile{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
