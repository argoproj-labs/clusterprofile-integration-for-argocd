package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cmdutil "github.com/argoproj/argo-cd/v3/cmd/util"
	"github.com/argoproj/argo-cd/v3/common"
	appv1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/cli"
	"github.com/argoproj/argo-cd/v3/util/env"
	"github.com/argoproj/argo-cd/v3/util/errors"
	logutils "github.com/argoproj/argo-cd/v3/util/log"

	log "github.com/sirupsen/logrus"
	clusterv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
)

const (
	cliName = "argocd-clusterprofile-controller"
	// allNamespacesSentinel ("*") mirrors Argo CD's --application-namespaces and requests a cluster-wide watch.
	allNamespacesSentinel = "*"
)

type cacheSyncReadiness struct {
	ready atomic.Bool
}

// Start reports ready for as long as the manager runs, because non-leader-election runnables
// start only after the cache has synced.
func (r *cacheSyncReadiness) Start(ctx context.Context) error {
	r.ready.Store(true)
	defer r.ready.Store(false)
	<-ctx.Done()
	return nil
}

func (*cacheSyncReadiness) NeedLeaderElection() bool {
	return false
}

func (r *cacheSyncReadiness) Check(_ *http.Request) error {
	if !r.ready.Load() {
		return fmt.Errorf("manager cache has not synced")
	}
	return nil
}

// registerCacheInformers registers an informer for every cached type. Controller sources acquire
// informers lazily when they start, so registering up front is what keeps the cache-sync gate
// from completing against an empty informer set.
func registerCacheInformers(ctx context.Context, mgr ctrl.Manager, cacheOpt cache.Options) error {
	for object := range cacheOpt.ByObject {
		if _, err := mgr.GetCache().GetInformer(
			ctx, object, cache.BlockUntilSynced(false),
		); err != nil {
			return fmt.Errorf("failed to register informer for %T: %w", object, err)
		}
	}
	return nil
}

func buildRESTConfig(clientConfig clientcmd.ClientConfig) (*rest.Config, error) {
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, err
	}

	vers := common.GetVersion()
	restConfig.UserAgent = fmt.Sprintf("%s/%s (%s)", cliName, vers.Version, vers.Platform)
	if err := appv1alpha1.SetK8SConfigDefaults(restConfig); err != nil {
		return nil, err
	}

	return restConfig, nil
}

func NewCommand() *cobra.Command {
	var (
		clientConfig               clientcmd.ClientConfig
		metricsAddr                string
		probeAddr                  string
		enableLeaderElection       bool
		clusterProfileNamespaces   []string
		debugLog                   bool
		dryRun                     bool
		clusterProfileProviderFile string
	)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clusterv1alpha1.AddToScheme(scheme)

	command := cobra.Command{
		Use:               cliName,
		Short:             "Starts Argo CD Cluster Profile Controller",
		DisableAutoGenTag: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			vers := common.GetVersion()
			namespace, _, err := clientConfig.Namespace()
			errors.CheckError(err)

			vers.LogStartupInfo(
				"ArgoCD Cluster Profile Controller",
				map[string]any{
					"namespace": namespace,
				},
			)

			cli.SetLogFormat(cmdutil.LogFormat)

			if debugLog {
				cli.SetLogLevel("debug")
			} else {
				cli.SetLogLevel(cmdutil.LogLevel)
			}

			ctrl.SetLogger(logutils.NewLogrusLogger(logutils.NewWithCurrentConfig()))

			defer func() {
				if r := recover(); r != nil {
					log.WithField("trace", string(debug.Stack())).Fatal("Recovered from panic: ", r)
				}
			}()

			restConfig, err := buildRESTConfig(clientConfig)
			errors.CheckError(err)

			cacheOpt := buildCacheOptions(namespace, clusterProfileNamespaces)
			// Keep enough of the Pod termination grace period in reserve for the
			// leader-election release request and process exit after runnables drain.
			gracefulShutdownTimeout := 15 * time.Second

			mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
				Scheme: scheme,
				Metrics: metricsserver.Options{
					BindAddress: metricsAddr,
				},
				Cache:                         cacheOpt,
				HealthProbeBindAddress:        probeAddr,
				LeaderElection:                enableLeaderElection,
				LeaderElectionID:              "clusterprofile.argoproj.io",
				LeaderElectionReleaseOnCancel: true,
				GracefulShutdownTimeout:       &gracefulShutdownTimeout,
				Client: ctrlclient.Options{
					DryRun: &dryRun,
				},
			})
			if err != nil {
				log.Error(err, "unable to start manager")
				os.Exit(1)
			}

			if err = (&ClusterProfileReconciler{
				Client:                     mgr.GetClient(),
				Scheme:                     mgr.GetScheme(),
				Log:                        ctrl.Log.WithName("controllers").WithName("ClusterProfile"),
				ClusterProfileProviderFile: clusterProfileProviderFile,
			}).SetupWithManager(mgr); err != nil {
				log.Error(err, "unable to create controller", "controller", "ClusterProfile")
				os.Exit(1)
			}

			if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
				log.Error(err, "unable to set up health check")
				os.Exit(1)
			}
			ctx := ctrl.SetupSignalHandler()
			if err := registerCacheInformers(ctx, mgr, cacheOpt); err != nil {
				log.Error(err, "unable to register cache informers")
				os.Exit(1)
			}
			cacheReadiness := &cacheSyncReadiness{}
			if err := mgr.Add(cacheReadiness); err != nil {
				log.Error(err, "unable to register cache readiness signal")
				os.Exit(1)
			}
			if err := mgr.AddReadyzCheck("cache-sync", cacheReadiness.Check); err != nil {
				log.Error(err, "unable to set up ready check")
				os.Exit(1)
			}

			log.Info("Starting manager")
			if err := mgr.Start(ctx); err != nil {
				log.Error(err, "problem running manager")
				os.Exit(1)
			}
			return nil
		},
	}
	clientConfig = cli.AddKubectlFlagsToCmd(&command)
	command.Flags().StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	command.Flags().StringVar(&probeAddr, "probe-addr", ":8081", "The address the probe endpoint binds to.")
	command.Flags().BoolVar(
		&enableLeaderElection,
		"enable-leader-election",
		env.ParseBoolFromEnv("ARGOCD_CLUSTERPROFILE_CONTROLLER_ENABLE_LEADER_ELECTION", false),
		"Enable leader election for controller manager. ",
	)
	command.Flags().StringSliceVar(
		&clusterProfileNamespaces,
		"cluster-profile-namespaces",
		env.StringsFromEnv("ARGOCD_CLUSTERPROFILE_CONTROLLER_NAMESPACES", []string{}, ","),
		"Comma-separated namespaces to watch for ClusterProfiles. "+
			"Use '*' to watch all namespaces (defaults to the active namespace).",
	)
	command.Flags().BoolVar(
		&debugLog,
		"debug",
		env.ParseBoolFromEnv("ARGOCD_CLUSTERPROFILE_CONTROLLER_DEBUG", false),
		"Print debug logs. Takes precedence over loglevel",
	)
	command.Flags().StringVar(
		&cmdutil.LogFormat,
		"logformat",
		env.StringFromEnv("ARGOCD_CLUSTERPROFILE_CONTROLLER_LOGFORMAT", "json"),
		"Set the logging format. One of: json|text",
	)
	command.Flags().StringVar(
		&cmdutil.LogLevel,
		"loglevel",
		env.StringFromEnv("ARGOCD_CLUSTERPROFILE_CONTROLLER_LOGLEVEL", "info"),
		"Set the logging level. One of: debug|info|warn|error",
	)
	command.Flags().BoolVar(
		&dryRun,
		"dry-run",
		env.ParseBoolFromEnv("ARGOCD_CLUSTERPROFILE_CONTROLLER_DRY_RUN", false),
		"Enable dry run mode",
	)
	command.Flags().StringVar(
		&clusterProfileProviderFile,
		"clusterprofile-provider-file",
		env.StringFromEnv("ARGOCD_CLUSTERPROFILE_CONTROLLER_CLUSTERPROFILE_PROVIDER_FILE", ""),
		"The path to the cluster profile provider file.",
	)

	return &command
}

func buildCacheOptions(
	controllerNamespace string,
	clusterProfileNamespaces []string,
) cache.Options {
	namespaces := normalizeNamespaces(clusterProfileNamespaces)
	if len(namespaces) == 0 {
		namespaces = []string{controllerNamespace}
	}

	// An empty map means every namespace, so only the sentinel leaves it empty.
	namespaceConfigs := map[string]cache.Config{}
	if !slices.Contains(namespaces, allNamespacesSentinel) {
		namespaceConfigs = namespaceCacheConfigs(namespaces)
	}

	return cache.Options{
		ByObject: map[ctrlclient.Object]cache.ByObject{
			&clusterv1alpha1.ClusterProfile{}: {
				Namespaces: namespaceConfigs,
			},
			&corev1.Secret{}: {
				Namespaces: namespaceConfigs,
			},
		},
	}
}

func namespaceCacheConfigs(namespaces []string) map[string]cache.Config {
	configs := make(map[string]cache.Config, len(namespaces))
	for _, namespace := range namespaces {
		configs[namespace] = cache.Config{}
	}
	return configs
}

// normalizeNamespaces trims surrounding whitespace and drops blank entries (e.g. from a trailing
// comma in the env var) so a stray "" never silently expands the watch scope to all namespaces.
func normalizeNamespaces(namespaces []string) []string {
	normalized := make([]string, 0, len(namespaces))
	for _, namespace := range namespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" {
			continue
		}
		normalized = append(normalized, namespace)
	}
	return normalized
}

func main() {
	command := NewCommand()
	if err := command.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
