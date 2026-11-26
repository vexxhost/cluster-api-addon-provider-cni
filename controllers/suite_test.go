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

package controllers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	helmv1 "sigs.k8s.io/cluster-api-addon-provider-helm/api/v1alpha1"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
)

var (
	env        *envtest.Environment
	envConfig  *rest.Config
	envClient  client.Client
	envScheme  = runtime.NewScheme()
	envEnabled bool
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("REQUIRE_ENVTEST") != "" {
			fmt.Fprintln(os.Stderr, "REQUIRE_ENVTEST is set but KUBEBUILDER_ASSETS is empty; run tests with `make test`")
			return 1
		}
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set, envtest integration tests will be skipped")
		return m.Run()
	}

	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, clusterv1.AddToScheme, addonsv1.AddToScheme, helmv1.AddToScheme, cniv1.AddToScheme,
	} {
		if err := add(envScheme); err != nil {
			panic(err)
		}
	}

	capiCRDs, err := moduleDir("sigs.k8s.io/cluster-api", "core", "config", "crd", "bases")
	if err != nil {
		panic(err)
	}
	caaphCRDs, err := moduleDir("sigs.k8s.io/cluster-api-addon-provider-helm", "config", "crd", "bases")
	if err != nil {
		panic(err)
	}
	env = &envtest.Environment{
		CRDDirectoryPaths:     []string{capiCRDs, caaphCRDs, filepath.Join("..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		Scheme:                envScheme,
	}
	envConfig, err = env.Start()
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "failed to stop envtest:", err)
		}
	}()

	envClient, err = client.New(envConfig, client.Options{Scheme: envScheme})
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.SetLogger(klog.Background())

	mgr, err := ctrl.NewManager(envConfig, ctrl.Options{
		Scheme:  envScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		panic(err)
	}
	cc, err := clustercache.SetupWithManager(ctx, mgr, clustercache.Options{
		SecretClient: mgr.GetClient(),
		Client:       clustercache.ClientOptions{UserAgent: ControllerName},
	}, controller.Options{MaxConcurrentReconciles: 10})
	if err != nil {
		panic(err)
	}
	catalogs, err := catalog.Builtin()
	if err != nil {
		panic(err)
	}
	if err := (&ClusterCNIReconciler{
		Client:       mgr.GetClient(),
		APIReader:    mgr.GetAPIReader(),
		ClusterCache: cc,
		Catalogs:     catalogs,
	}).SetupWithManager(ctx, mgr, controller.Options{MaxConcurrentReconciles: 10}); err != nil {
		panic(err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic(err)
		}
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		panic("manager cache did not sync")
	}

	envEnabled = true
	return m.Run()
}

func requireEnvtest(t *testing.T) {
	t.Helper()
	if !envEnabled {
		t.Skip("envtest is not available, set KUBEBUILDER_ASSETS to run integration tests")
	}
}

// moduleDir returns a directory inside a module from the module cache, so
// CRDs always match the versions in go.mod.
func moduleDir(module string, elem ...string) (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", module).Output()
	if err != nil {
		return "", fmt.Errorf("failed to locate %s: %w", module, err)
	}
	return filepath.Join(append([]string{strings.TrimSpace(string(out))}, elem...)...), nil
}

// eventually is the default timing for integration tests.
var eventually = []any{30 * time.Second, 200 * time.Millisecond}
