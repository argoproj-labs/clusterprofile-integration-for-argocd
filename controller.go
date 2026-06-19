package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clusterinventory "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/argoproj/argo-cd/v3/common"
	v1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const (
	clusterProfileFinalizer = "argoproj.io/cluster-profile-finalizer"
	secretNameTemplate      = "cluster-%s"
	// clusterProfileOriginLabel records the source ClusterProfile (as "namespace-name") on the generated Secret.
	clusterProfileOriginLabel = "argocd.argoproj.io/cluster-profile-origin"
	secretDataNameKey         = "name"
	secretDataServerKey       = "server"
	secretDataConfigKey       = "config"
	maxSecretNameLength       = validation.DNS1123SubdomainMaxLength
	secretNameHashLength      = 10
)

// ClusterProfileReconciler reconciles a ClusterProfile object with a corresponding Secret
type ClusterProfileReconciler struct {
	client.Client
	Log       logr.Logger
	Scheme    *runtime.Scheme
	Namespace string
	// ClusterProfileProviderFile is the path to the file containing the cluster profile provider configuration.
	ClusterProfileProviderFile string
	// AccessProviders is the set of access providers used to build the kubeconfig for a ClusterProfile.
	AccessProviders *access.Config
}

//+kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=clusterprofiles,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *ClusterProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("clusterprofile", req.NamespacedName)

	var clusterProfile clusterinventory.ClusterProfile
	if err := r.Get(ctx, req.NamespacedName, &clusterProfile); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch ClusterProfile")
		return ctrl.Result{}, err
	}

	if !clusterProfile.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.pruneSecret(ctx, &clusterProfile)
	}

	if !controllerutil.ContainsFinalizer(&clusterProfile, clusterProfileFinalizer) {
		controllerutil.AddFinalizer(&clusterProfile, clusterProfileFinalizer)
		if err := r.Update(ctx, &clusterProfile); err != nil {
			log.Error(err, "unable to update ClusterProfile")
			return ctrl.Result{}, err
		}
	}

	secretName := clusterProfileSecretName(&clusterProfile)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: r.Namespace,
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

// pruneSecret handles the deletion of the Secret associated with a ClusterProfile.
func (r *ClusterProfileReconciler) pruneSecret(
	ctx context.Context,
	clusterProfile *clusterinventory.ClusterProfile,
) error {
	log := r.Log.WithValues("clusterprofile", clusterProfile.Name)

	// If the finalizer is gone, the secret has already been pruned.
	if !controllerutil.ContainsFinalizer(clusterProfile, clusterProfileFinalizer) {
		return nil
	}

	secretName := clusterProfileSecretName(clusterProfile)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: r.Namespace,
		},
	}

	err := r.Delete(ctx, secret)
	if err != nil && !errors.IsNotFound(err) {
		log.Error(err, "unable to delete secret")
		return err
	}

	controllerutil.RemoveFinalizer(clusterProfile, clusterProfileFinalizer)
	if err := r.Update(ctx, clusterProfile); err != nil {
		log.Error(err, "unable to update ClusterProfile for deletion")
		return err
	}
	return nil
}

// mutateSecret populates the secret with the labels and cluster config derived from the ClusterProfile.
func (r *ClusterProfileReconciler) mutateSecret(
	secret *corev1.Secret,
	clusterProfile *clusterinventory.ClusterProfile,
) error {
	clusterName := clusterProfileClusterName(clusterProfile)
	labels := make(map[string]string, len(clusterProfile.Labels)+2)
	maps.Copy(labels, clusterProfile.Labels)
	labels[common.LabelKeySecretType] = common.LabelValueSecretTypeCluster
	labels[clusterProfileOriginLabel] = clusterName
	secret.Labels = labels

	clusterConfig, server, err := r.buildClusterConfig(clusterProfile)
	if err != nil {
		return err
	}
	configBytes, err := json.Marshal(clusterConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	secret.StringData = map[string]string{
		secretDataNameKey:   clusterName,
		secretDataServerKey: server,
		secretDataConfigKey: string(configBytes),
	}
	return nil
}

func clusterProfileClusterName(clusterProfile *clusterinventory.ClusterProfile) string {
	return fmt.Sprintf("%s-%s", clusterProfile.Namespace, clusterProfile.Name)
}

func clusterProfileSecretName(clusterProfile *clusterinventory.ClusterProfile) string {
	return truncateSecretName(fmt.Sprintf(secretNameTemplate, clusterProfileClusterName(clusterProfile)))
}

func truncateSecretName(name string) string {
	if len(name) <= maxSecretNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + fmt.Sprintf("%x", sum)[:secretNameHashLength]
	// Trim trailing separators so the truncated prefix doesn't produce a doubled "--" or "-." before the suffix.
	prefix := strings.TrimRight(name[:maxSecretNameLength-len(suffix)], "-.")
	return prefix + suffix
}

// buildClusterConfig resolves a ClusterProfile into the Argo CD cluster config and server URL.
// A ClusterProfile exposing an "argo-cd-builtin-<cloud>" access provider authenticates directly via
// argocd-k8s-auth (e.g. cmd/argocd-k8s-auth/commands/gcp.go); any other provider is resolved through
// the configured access providers.
func (r *ClusterProfileReconciler) buildClusterConfig(
	clusterProfile *clusterinventory.ClusterProfile,
) (v1alpha1.ClusterConfig, string, error) {
	for _, provider := range clusterProfile.Status.AccessProviders {
		if !strings.HasPrefix(provider.Name, "argo-cd-builtin-") {
			continue
		}
		cloudProvider := strings.TrimPrefix(provider.Name, "argo-cd-builtin-")
		return v1alpha1.ClusterConfig{
			ExecProviderConfig: &v1alpha1.ExecProviderConfig{
				Command:    "argocd-k8s-auth",
				Args:       []string{cloudProvider},
				APIVersion: "client.authentication.k8s.io/v1beta1",
			},
		}, provider.Cluster.Server, nil
	}

	if r.AccessProviders == nil {
		return v1alpha1.ClusterConfig{}, "", fmt.Errorf(
			"access providers not configured for cluster profile %q", clusterProfile.Name)
	}

	config, err := cloneAccessConfig(r.AccessProviders).BuildConfigFromCP(clusterProfile)
	if err != nil {
		return v1alpha1.ClusterConfig{}, "", fmt.Errorf("failed to build config: %w", err)
	}

	clusterConfig := v1alpha1.ClusterConfig{
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
	if config.ExecProvider != nil {
		replacer := strings.NewReplacer(
			"{{ .ClusterProfileName }}", clusterProfile.Name,
			"{{ .ClusterProfileServer }}", config.Host,
		)
		args := make([]string, len(config.ExecProvider.Args))
		for i, arg := range config.ExecProvider.Args {
			args[i] = replacer.Replace(arg)
		}
		clusterConfig.ExecProviderConfig = &v1alpha1.ExecProviderConfig{
			Command:            config.ExecProvider.Command,
			Args:               args,
			APIVersion:         config.ExecProvider.APIVersion,
			ProvideClusterInfo: config.ExecProvider.ProvideClusterInfo,
		}
		if len(config.ExecProvider.Env) > 0 {
			env := make(map[string]string, len(config.ExecProvider.Env))
			for _, e := range config.ExecProvider.Env {
				env[e.Name] = e.Value
			}
			clusterConfig.ExecProviderConfig.Env = env
		}
		// Preserve the exec provider Config (e.g. clusterName) sourced from the ClusterProfile's
		// cluster.extensions "client.authentication.k8s.io/exec" key, per the Kubernetes client
		// authentication API: https://kubernetes.io/docs/reference/config-api/kubeconfig.v1/#ExecConfig
		if config.ExecProvider.Config != nil {
			if configData, err := json.Marshal(config.ExecProvider.Config); err == nil {
				clusterConfig.ExecProviderConfig.Config = &runtime.RawExtension{Raw: configData}
			}
		}
	}
	return clusterConfig, config.Host, nil
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
