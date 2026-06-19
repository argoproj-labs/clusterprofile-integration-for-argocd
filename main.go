package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
)

func NewCommand() *cobra.Command {
	var (
		clientConfig                clientcmd.ClientConfig
		metricsAddr                 string
		probeAddr                   string
		enableLeaderElection        bool
		clusterProfileNamespaces    []string
		clusterProfileAllNamespaces bool
		debugLog                    bool
		dryRun                      bool
		clusterProfileProviderFile  string
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

			if clusterProfileAllNamespaces && len(clusterProfileNamespaces) > 0 {
				errors.CheckError(fmt.Errorf(
					"can't use both --cluster-profile-all-namespaces and --cluster-profile-namespaces",
				))
			}

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

			cfg := ctrl.GetConfigOrDie()
			cfg.UserAgent = fmt.Sprintf("argocd-clusterprofile-controller/%s (%s)", vers.Version, vers.Platform)
			if err = appv1alpha1.SetK8SConfigDefaults(cfg); err != nil {
				log.Error(err, "Unable to apply K8s REST config defaults")
				os.Exit(1)
			}

			cacheOpt := buildCacheOptions(namespace, clusterProfileNamespaces, clusterProfileAllNamespaces)

			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme: scheme,
				Metrics: metricsserver.Options{
					BindAddress: metricsAddr,
				},
				Cache:                  cacheOpt,
				HealthProbeBindAddress: probeAddr,
				LeaderElection:         enableLeaderElection,
				LeaderElectionID:       "clusterprofile.argoproj.io",
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
				Namespace:                  namespace,
				ClusterProfileProviderFile: clusterProfileProviderFile,
			}).SetupWithManager(mgr); err != nil {
				log.Error(err, "unable to create controller", "controller", "ClusterProfile")
				os.Exit(1)
			}

			if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
				log.Error(err, "unable to set up health check")
				os.Exit(1)
			}
			if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
				log.Error(err, "unable to set up ready check")
				os.Exit(1)
			}

			log.Info("Starting manager")
			if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
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
		"Argo CD cluster profile namespaces",
	)
	command.Flags().BoolVar(
		&clusterProfileAllNamespaces,
		"cluster-profile-all-namespaces",
		env.ParseBoolFromEnv("ARGOCD_CLUSTERPROFILE_CONTROLLER_ALL_NAMESPACES", false),
		"Watch ClusterProfile resources in all namespaces",
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

// buildCacheOptions scopes Secrets to the controller namespace and ClusterProfiles to the
// requested namespaces (all namespaces when clusterProfileAllNamespaces is set).
func buildCacheOptions(
	controllerNamespace string,
	clusterProfileNamespaces []string,
	clusterProfileAllNamespaces bool,
) cache.Options {
	controllerNamespaces := map[string]cache.Config{controllerNamespace: {}}

	clusterProfileNamespaceConfigs := map[string]cache.Config{}
	if !clusterProfileAllNamespaces {
		namespaces := clusterProfileNamespaces
		if len(namespaces) == 0 {
			namespaces = []string{controllerNamespace}
		}
		for _, namespace := range namespaces {
			clusterProfileNamespaceConfigs[namespace] = cache.Config{}
		}
	}

	return cache.Options{
		DefaultNamespaces: controllerNamespaces,
		ByObject: map[ctrlclient.Object]cache.ByObject{
			&clusterv1alpha1.ClusterProfile{}: {Namespaces: clusterProfileNamespaceConfigs},
			&corev1.Secret{}:                  {Namespaces: controllerNamespaces},
		},
	}
}

func main() {
	command := NewCommand()
	if err := command.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
