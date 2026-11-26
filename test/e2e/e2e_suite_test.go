//go:build e2e

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

package e2e

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1 "sigs.k8s.io/cluster-api-addon-provider-helm/api/v1alpha1"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimev1 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/bootstrap"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
)

var (
	ctx = ctrl.SetupSignalHandler()

	// Flags.
	configPath         string
	artifactFolder     string
	skipCleanup        bool
	useExistingCluster bool

	// Shared state, set up once in SynchronizedBeforeSuite.
	e2eConfig                *clusterctl.E2EConfig
	clusterctlConfigPath     string
	bootstrapClusterProvider bootstrap.ClusterProvider
	bootstrapClusterProxy    framework.ClusterProxy
)

func init() {
	flag.StringVar(&configPath, "e2e.config", "", "path to the e2e config file")
	flag.StringVar(&artifactFolder, "e2e.artifacts-folder", "", "folder where e2e test artifact should be stored")
	flag.BoolVar(&skipCleanup, "e2e.skip-resource-cleanup", false, "if true, the resource cleanup after tests will be skipped")
	flag.BoolVar(&useExistingCluster, "e2e.use-existing-cluster", false, "if true, the test uses the current cluster instead of creating a new one (default discovery rules apply)")
}

func TestE2E(t *testing.T) {
	ctrl.SetLogger(klog.Background())
	RegisterFailHandler(Fail)
	RunSpecs(t, "cluster-api-addon-provider-cni-e2e")
}

var _ = SynchronizedBeforeSuite(func() []byte {
	Expect(configPath).To(BeAnExistingFile(), "Invalid test suite argument. e2e.config should be an existing file.")
	Expect(os.MkdirAll(artifactFolder, 0o755)).To(Succeed(), "Invalid test suite argument. Can't create e2e.artifacts-folder %q", artifactFolder)

	By("Copying the quick-start ClusterClass from the Cluster API test module")
	copyQuickStartClusterClass()

	By("Loading the e2e test configuration from " + configPath)
	e2eConfig = clusterctl.LoadE2EConfig(ctx, clusterctl.LoadE2EConfigInput{ConfigPath: configPath})
	Expect(e2eConfig).NotTo(BeNil())

	By("Creating a clusterctl local repository into " + artifactFolder)
	clusterctlConfigPath = clusterctl.CreateRepository(ctx, clusterctl.CreateRepositoryInput{
		E2EConfig:        e2eConfig,
		RepositoryFolder: filepath.Join(artifactFolder, "repository"),
	})
	Expect(clusterctlConfigPath).To(BeAnExistingFile())

	By("Setting up the bootstrap cluster")
	kubeconfigPath := ""
	if !useExistingCluster {
		bootstrapClusterProvider = bootstrap.CreateKindBootstrapClusterAndLoadImages(ctx, bootstrap.CreateKindBootstrapClusterAndLoadImagesInput{
			Name:               e2eConfig.ManagementClusterName,
			RequiresDockerSock: e2eConfig.HasDockerProvider(),
			Images:             e2eConfig.Images,
			LogFolder:          filepath.Join(artifactFolder, "kind"),
		})
		Expect(bootstrapClusterProvider).NotTo(BeNil(), "Failed to create a bootstrap cluster")
		kubeconfigPath = bootstrapClusterProvider.GetKubeconfigPath()
	}
	bootstrapClusterProxy = newBootstrapClusterProxy(kubeconfigPath)
	Expect(bootstrapClusterProxy).NotTo(BeNil())

	By("Initializing the bootstrap cluster with Cluster API, CAPD, CAAPH and cluster-api-addon-provider-cni")
	clusterctl.InitManagementClusterAndWatchControllerLogs(ctx, clusterctl.InitManagementClusterAndWatchControllerLogsInput{
		ClusterProxy:            bootstrapClusterProxy,
		ClusterctlConfigPath:    clusterctlConfigPath,
		InfrastructureProviders: e2eConfig.InfrastructureProviders(),
		AddonProviders:          e2eConfig.AddonProviders(),
		LogFolder:               filepath.Join(artifactFolder, "clusters", bootstrapClusterProxy.GetName()),
	}, e2eConfig.GetIntervals(bootstrapClusterProxy.GetName(), "wait-controllers")...)

	By("Registering the lifecycle hooks with the ExtensionConfig from config/runtime-extension")
	registerLifecycleHooks()

	return []byte(strings.Join([]string{artifactFolder, configPath, clusterctlConfigPath, bootstrapClusterProxy.GetKubeconfigPath()}, ","))
}, func(data []byte) {
	parts := strings.Split(string(data), ",")
	Expect(parts).To(HaveLen(4))

	artifactFolder = parts[0]
	e2eConfig = clusterctl.LoadE2EConfig(ctx, clusterctl.LoadE2EConfigInput{ConfigPath: parts[1]})
	clusterctlConfigPath = parts[2]
	bootstrapClusterProxy = newBootstrapClusterProxy(parts[3])
})

var _ = SynchronizedAfterSuite(func() {}, func() {
	if skipCleanup {
		return
	}
	By("Tearing down the management cluster")
	if bootstrapClusterProxy != nil {
		bootstrapClusterProxy.Dispose(ctx)
	}
	if bootstrapClusterProvider != nil {
		bootstrapClusterProvider.Dispose(ctx)
	}
})

// copyQuickStartClusterClass copies the quick-start ClusterClass of the
// Cluster API version in go.mod next to the cluster templates, where clusterctl
// finds it for the templates that reference it.
func copyQuickStartClusterClass() {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/cluster-api/test").Output()
	Expect(err).ToNot(HaveOccurred(), "locating the Cluster API test module")
	src := filepath.Join(strings.TrimSpace(string(out)), "e2e", "data", "infrastructure-docker", "main", "clusterclass-quick-start.yaml")
	data, err := os.ReadFile(src)
	Expect(err).ToNot(HaveOccurred())
	dst := filepath.Join(filepath.Dir(configPath), "..", "data", "infrastructure-docker", "clusterclass-quick-start.yaml")
	Expect(os.WriteFile(dst, data, 0o600)).To(Succeed())
}

// registerLifecycleHooks applies the ExtensionConfig and waits for Cluster
// API to discover the hooks.
func registerLifecycleHooks() {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), "..", "..", "..", "config", "runtime-extension", "extensionconfig.yaml"))
	Expect(err).ToNot(HaveOccurred())
	Expect(bootstrapClusterProxy.CreateOrUpdate(ctx, data)).To(Succeed())

	Eventually(func(g Gomega) {
		ext := &runtimev1.ExtensionConfig{}
		g.Expect(bootstrapClusterProxy.GetClient().Get(ctx, client.ObjectKey{Name: "capi-cni"}, ext)).To(Succeed())
		g.Expect(meta.IsStatusConditionTrue(ext.Status.Conditions, runtimev1.ExtensionConfigDiscoveredCondition)).To(BeTrue(), "conditions: %v", ext.Status.Conditions)
		g.Expect(ext.Status.Handlers).To(HaveLen(3))
	}, e2eConfig.GetIntervals(bootstrapClusterProxy.GetName(), "wait-controllers")...).Should(Succeed())
}

func newBootstrapClusterProxy(kubeconfigPath string) framework.ClusterProxy {
	var opts []framework.Option
	if e2eConfig.HasDockerProvider() {
		opts = append(opts, framework.WithMachineLogCollector(framework.DockerLogCollector{}))
	}
	return framework.NewClusterProxy("bootstrap", kubeconfigPath, initScheme(), opts...)
}

func initScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	framework.TryAddDefaultSchemes(scheme)
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	Expect(addonsv1.AddToScheme(scheme)).To(Succeed())
	Expect(runtimev1.AddToScheme(scheme)).To(Succeed())
	Expect(helmv1.AddToScheme(scheme)).To(Succeed())
	Expect(cniv1.AddToScheme(scheme)).To(Succeed())
	return scheme
}
