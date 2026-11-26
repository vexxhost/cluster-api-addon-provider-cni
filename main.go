/*
Copyright 2026 VEXXHOST, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The cluster-api-addon-provider-cni manager installs, upgrades and takes
// over the CNI of Cluster API workload clusters, and serves lifecycle hooks
// that keep the CNI in step with Kubernetes upgrades.
package main

import (
	"flag"
	"fmt"
	"os"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	logsv1 "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	helmv1 "sigs.k8s.io/cluster-api-addon-provider-helm/api/v1alpha1"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/exp/runtime/server"
	"sigs.k8s.io/cluster-api/util/apiwarnings"
	capicontrollerutil "sigs.k8s.io/cluster-api/util/controller"
	"sigs.k8s.io/cluster-api/util/flags"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/cluster-api/version"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
	"github.com/vexxhost/cluster-api-addon-provider-cni/controllers"
	"github.com/vexxhost/cluster-api-addon-provider-cni/hooks"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	enableLeaderElection        bool
	leaderElectionLeaseDuration time.Duration
	leaderElectionRenewDeadline time.Duration
	leaderElectionRetryPeriod   time.Duration
	watchFilterValue            string
	watchNamespace              string
	profilerAddress             string
	enableContentionProfiling   bool
	syncPeriod                  time.Duration
	restConfigQPS               float32
	restConfigBurst             int
	clusterCacheClientQPS       float32
	clusterCacheClientBurst     int
	clusterCacheConcurrency     int
	clusterCNIConcurrency       int
	healthAddr                  string
	webhookPort                 int
	webhookCertDir              string
	managerOptions              = flags.ManagerOptions{}
	logOptions                  = logs.NewOptions()
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = addonsv1.AddToScheme(scheme)
	_ = helmv1.AddToScheme(scheme)
	_ = cniv1.AddToScheme(scheme)
}

// InitFlags initializes the flags.
func InitFlags(fs *pflag.FlagSet) {
	logsv1.AddFlags(logOptions, fs)

	fs.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	fs.DurationVar(&leaderElectionLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"Interval at which non-leader candidates will wait to force acquire leadership (duration string)")
	fs.DurationVar(&leaderElectionRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"Duration that the leading controller manager will retry refreshing leadership before giving up (duration string)")
	fs.DurationVar(&leaderElectionRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"Duration the LeaderElector clients should wait between tries of actions (duration string)")

	fs.StringVar(&watchNamespace, "namespace", "",
		"Namespace that the controller watches to reconcile cluster-api objects. If unspecified, the controller watches for cluster-api objects across all namespaces.")
	fs.StringVar(&watchFilterValue, "watch-filter", "",
		fmt.Sprintf("Label value that the controller watches to reconcile cluster-api objects. Label key is always %s. If unspecified, the controller watches for all cluster-api objects.", clusterv1.WatchLabel))

	fs.StringVar(&profilerAddress, "profiler-address", "",
		"Bind address to expose the pprof profiler (e.g. localhost:6060)")
	fs.BoolVar(&enableContentionProfiling, "contention-profiling", false,
		"Enable block profiling")
	fs.IntVar(&clusterCacheConcurrency, "clustercache-concurrency", 100,
		"Number of clusters to process simultaneously")
	fs.IntVar(&clusterCNIConcurrency, "clustercni-concurrency", 10,
		"Number of ClusterCNIs to process simultaneously")
	fs.DurationVar(&syncPeriod, "sync-period", 10*time.Minute,
		"The minimum interval at which watched resources are reconciled (e.g. 15m)")
	fs.Float32Var(&restConfigQPS, "kube-api-qps", 20,
		"Maximum queries per second from the controller client to the Kubernetes API server.")
	fs.IntVar(&restConfigBurst, "kube-api-burst", 30,
		"Maximum number of queries that should be allowed in one burst from the controller client to the Kubernetes API server.")
	fs.Float32Var(&clusterCacheClientQPS, "clustercache-client-qps", 20,
		"Maximum queries per second from the cluster cache clients to the Kubernetes API server of workload clusters.")
	fs.IntVar(&clusterCacheClientBurst, "clustercache-client-burst", 30,
		"Maximum number of queries that should be allowed in one burst from the cluster cache clients to the Kubernetes API server of workload clusters.")
	fs.StringVar(&healthAddr, "health-addr", ":9440",
		"The address the health endpoint binds to.")
	fs.IntVar(&webhookPort, "webhook-port", server.DefaultPort,
		"Port the lifecycle hook server listens on.")
	fs.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs/",
		"Directory with the serving certificate (tls.crt and tls.key) of the lifecycle hook server.")

	flags.AddManagerOptions(fs, &managerOptions)
}

// Add RBAC for the authorized diagnostics endpoint.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

func main() {
	InitFlags(pflag.CommandLine)
	pflag.CommandLine.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	if err := pflag.CommandLine.Set("v", "2"); err != nil {
		fmt.Printf("Failed to set default log level: %v\n", err)
		os.Exit(1)
	}
	pflag.Parse()

	if err := logsv1.ValidateAndApply(logOptions, nil); err != nil {
		fmt.Printf("Unable to start manager: %v\n", err)
		os.Exit(1)
	}
	ctrl.SetLogger(klog.Background())
	setupLog.Info(fmt.Sprintf("Version: %s (git commit: %s)", version.Get().String(), version.Get().GitCommit))

	catalogs, err := catalog.Builtin()
	if err != nil {
		setupLog.Error(err, "Invalid built-in catalog")
		os.Exit(1)
	}

	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS = restConfigQPS
	restConfig.Burst = restConfigBurst
	restConfig.UserAgent = remote.DefaultClusterAPIUserAgent(controllers.ControllerName)
	restConfig.WarningHandler = apiwarnings.DefaultHandler(klog.Background().WithName("API Server Warning"))

	_, metricsOptions, err := flags.GetManagerOptions(managerOptions)
	if err != nil {
		setupLog.Error(err, "Unable to start manager: invalid flags")
		os.Exit(1)
	}

	if enableContentionProfiling {
		goruntime.SetBlockProfileRate(1)
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                     scheme,
		LeaderElection:             enableLeaderElection,
		LeaderElectionID:           "cluster-api-addon-provider-cni-leader-election",
		LeaseDuration:              &leaderElectionLeaseDuration,
		RenewDeadline:              &leaderElectionRenewDeadline,
		RetryPeriod:                &leaderElectionRetryPeriod,
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		HealthProbeBindAddress:     healthAddr,
		PprofBindAddress:           profilerAddress,
		Metrics:                    *metricsOptions,
		Cache:                      managerCacheOptions(watchNamespace, syncPeriod),
		Client: client.Options{
			Cache: &client.CacheOptions{
				// Secrets are only read through the secret caching client below.
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to create ready check")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to create health check")
		os.Exit(1)
	}

	secretCachingClient, err := client.New(mgr.GetConfig(), client.Options{
		HTTPClient: mgr.GetHTTPClient(),
		Cache:      &client.CacheOptions{Reader: mgr.GetCache()},
	})
	if err != nil {
		setupLog.Error(err, "Unable to create secret caching client")
		os.Exit(1)
	}

	clusterCache, err := clustercache.SetupWithManager(ctx, mgr, clustercache.Options{
		SecretClient:     secretCachingClient,
		WatchFilterValue: watchFilterValue,
		Client: clustercache.ClientOptions{
			QPS:       clusterCacheClientQPS,
			Burst:     clusterCacheClientBurst,
			UserAgent: remote.DefaultClusterAPIUserAgent(controllers.ControllerName),
		},
	}, controller.Options{MaxConcurrentReconciles: clusterCacheConcurrency})
	if err != nil {
		setupLog.Error(err, "Unable to create ClusterCache")
		os.Exit(1)
	}

	if err := (&controllers.ClusterCNIReconciler{
		Client:           mgr.GetClient(),
		APIReader:        mgr.GetAPIReader(),
		ClusterCache:     clusterCache,
		Catalogs:         catalogs,
		WatchFilterValue: watchFilterValue,
	}).SetupWithManager(ctx, mgr, controller.Options{MaxConcurrentReconciles: clusterCNIConcurrency}); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", controllers.ControllerName)
		os.Exit(1)
	}

	runtimeCatalog := runtimecatalog.New()
	if err := runtimehooksv1.AddToCatalog(runtimeCatalog); err != nil {
		setupLog.Error(err, "Unable to build the runtime hook catalog")
		os.Exit(1)
	}
	hookServer, err := server.New(server.Options{Catalog: runtimeCatalog, Port: webhookPort, CertDir: webhookCertDir})
	if err != nil {
		setupLog.Error(err, "Unable to create the lifecycle hook server")
		os.Exit(1)
	}
	if err := (&hooks.Handlers{Client: mgr.GetClient(), Catalogs: catalogs}).Register(hookServer); err != nil {
		setupLog.Error(err, "Unable to register lifecycle hooks")
		os.Exit(1)
	}
	if err := mgr.Add(hookServer); err != nil {
		setupLog.Error(err, "Unable to add the lifecycle hook server")
		os.Exit(1)
	}

	setupLog.Info("Starting manager", "version", version.Get().String())
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}

// managerCacheOptions caches only Cluster API kubeconfig Secrets and drops the
// data of any other Secret it sees, like the Cluster API controllers do.
func managerCacheOptions(watchNamespace string, syncPeriod time.Duration) ctrlcache.Options {
	var namespaces map[string]ctrlcache.Config
	if watchNamespace != "" {
		namespaces = map[string]ctrlcache.Config{watchNamespace: {}}
	}

	req, _ := labels.NewRequirement(clusterv1.ClusterNameLabel, selection.Exists, nil)

	return ctrlcache.Options{
		DefaultNamespaces: namespaces,
		SyncPeriod:        &syncPeriod,
		DefaultTransform:  ctrlcache.TransformStripManagedFields(),
		ByObject: map[client.Object]ctrlcache.ByObject{
			&corev1.Secret{}: {
				Label: labels.NewSelector().Add(*req),
				Transform: func(in any) (any, error) {
					if s, ok := in.(*corev1.Secret); ok {
						s.SetManagedFields(nil)
						if !strings.HasSuffix(s.Name, "-"+string(secret.Kubeconfig)) {
							s.Data = nil
						}
					}
					return in, nil
				},
			},
		},
		NewInformer: capicontrollerutil.NewInformerFunc(scheme, controllers.ControllerName),
	}
}
