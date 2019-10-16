package main

import (
	"context"
	"fmt"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	// load the gcp plugin (required to authenticate against GKE clusters).
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	// load the oidc plugin (required to authenticate with OpenID Connect).
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"

	"github.com/argoproj/argo-cd/common"
	"github.com/argoproj/argo-cd/controller"
	"github.com/argoproj/argo-cd/engine"
	"github.com/argoproj/argo-cd/errors"
	"github.com/argoproj/argo-cd/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/reposerver/apiclient"
	"github.com/argoproj/argo-cd/util"
	"github.com/argoproj/argo-cd/util/argo"
	"github.com/argoproj/argo-cd/util/cache"
	"github.com/argoproj/argo-cd/util/cli"
	"github.com/argoproj/argo-cd/util/db"
	"github.com/argoproj/argo-cd/util/kube"
	"github.com/argoproj/argo-cd/util/settings"
	"github.com/argoproj/argo-cd/util/stats"
)

const (
	// CLIName is the name of the CLI
	cliName = "argocd-application-controller"
	// Default time in seconds for application resync period
	defaultAppResyncPeriod = 180
)

type grpcManifestGenerator struct {
	repoClientset apiclient.Clientset
}

func (g *grpcManifestGenerator) Generate(
	ctx context.Context,
	repo *v1alpha1.Repository,
	revision string,
	source *v1alpha1.ApplicationSource,
	setting *engine.ManifestGenerationSettings,
) (*engine.ManifestResponse, error) {
	conn, repoClient, err := g.repoClientset.NewRepoServerClient()
	if err != nil {
		return nil, err
	}
	defer util.Close(conn)
	res, err := repoClient.GenerateManifest(ctx, &apiclient.ManifestRequest{
		Repo:              repo,
		Revision:          revision,
		NoCache:           setting.NoCache,
		AppLabelKey:       setting.AppLabelKey,
		AppLabelValue:     setting.AppLabelValue,
		Namespace:         setting.Namespace,
		ApplicationSource: source,
		Repos:             setting.Repos,
		Plugins:           setting.Plugins,
		KustomizeOptions:  setting.KustomizeOptions,
		KubeVersion:       setting.KubeVersion,
	})
	if err != nil {
		return nil, err
	}
	return &engine.ManifestResponse{
		Namespace:  res.Namespace,
		Server:     res.Server,
		Revision:   res.Revision,
		Manifests:  res.Manifests,
		SourceType: res.SourceType,
	}, nil
}

func newCommand() *cobra.Command {
	var (
		clientConfig             clientcmd.ClientConfig
		appResyncPeriod          int64
		repoServerAddress        string
		repoServerTimeoutSeconds int
		selfHealTimeoutSeconds   int
		statusProcessors         int
		operationProcessors      int
		logLevel                 string
		glogLevel                int
		metricsPort              int
		kubectlParallelismLimit  int64
		cacheSrc                 func() (*cache.Cache, error)
	)
	var command = cobra.Command{
		Use:   cliName,
		Short: "application-controller is a controller to operate on applications CRD",
		RunE: func(c *cobra.Command, args []string) error {
			cli.SetLogLevel(logLevel)
			cli.SetGLogLevel(glogLevel)

			config, err := clientConfig.ClientConfig()
			errors.CheckError(err)
			config.QPS = common.K8sClientConfigQPS
			config.Burst = common.K8sClientConfigBurst

			kubeClient := kubernetes.NewForConfigOrDie(config)
			appClient := appclientset.NewForConfigOrDie(config)

			namespace, _, err := clientConfig.Namespace()
			errors.CheckError(err)

			resyncDuration := time.Duration(appResyncPeriod) * time.Second
			repoClientset := apiclient.NewRepoServerClientset(repoServerAddress, repoServerTimeoutSeconds)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			cache, err := cacheSrc()
			errors.CheckError(err)

			settingsMgr := settings.NewSettingsManager(ctx, kubeClient, namespace)
			kubectl := kube.KubectlCmd{}
			appController, err := controller.NewApplicationController(
				namespace,
				settingsMgr,
				db.NewDB(namespace, settingsMgr, kubeClient),
				argo.NewAuditLogger(namespace, kubeClient, "argocd-application-controller"),
				appClient,
				&grpcManifestGenerator{repoClientset: repoClientset},
				cache,
				kubectl,
				resyncDuration,
				time.Duration(selfHealTimeoutSeconds)*time.Second,
				metricsPort,
				kubectlParallelismLimit, func() error {
					_, err := kubeClient.Discovery().ServerVersion()
					return err
				})
			errors.CheckError(err)

			log.Infof("Application Controller (version: %s) starting (namespace: %s)", common.GetVersion(), namespace)
			stats.RegisterStackDumper()
			stats.StartStatsTicker(10 * time.Minute)
			stats.RegisterHeapDumper("memprofile")

			go appController.Run(ctx, statusProcessors, operationProcessors)

			// Wait forever
			select {}
		},
	}

	clientConfig = cli.AddKubectlFlagsToCmd(&command)
	command.Flags().Int64Var(&appResyncPeriod, "app-resync", defaultAppResyncPeriod, "Time period in seconds for application resync.")
	command.Flags().StringVar(&repoServerAddress, "repo-server", common.DefaultRepoServerAddr, "Repo server address.")
	command.Flags().IntVar(&repoServerTimeoutSeconds, "repo-server-timeout-seconds", 60, "Repo server RPC call timeout seconds.")
	command.Flags().IntVar(&statusProcessors, "status-processors", 1, "Number of application status processors")
	command.Flags().IntVar(&operationProcessors, "operation-processors", 1, "Number of application operation processors")
	command.Flags().StringVar(&logLevel, "loglevel", "info", "Set the logging level. One of: debug|info|warn|error")
	command.Flags().IntVar(&glogLevel, "gloglevel", 0, "Set the glog logging level")
	command.Flags().IntVar(&metricsPort, "metrics-port", common.DefaultPortArgoCDMetrics, "Start metrics server on given port")
	command.Flags().IntVar(&selfHealTimeoutSeconds, "self-heal-timeout-seconds", 5, "Specifies timeout between application self heal attempts")
	command.Flags().Int64Var(&kubectlParallelismLimit, "kubectl-parallelism-limit", 20, "Number of allowed concurrent kubectl fork/execs. Any value less the 1 means no limit.")

	cacheSrc = cache.AddCacheFlagsToCmd(&command)
	return &command
}

func main() {
	if err := newCommand().Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
