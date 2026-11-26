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

package hooks

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
)

// testCatalog stays fixed so these tests don't change with the built-in catalog.
const testCatalog = `
type: cilium
chart: {repoURL: https://charts.example.com, name: cilium}
release: {name: cilium, namespace: kube-system}
detect: [{kind: DaemonSet, namespace: kube-system, name: cilium, container: cilium-agent}]
valuesTemplate: "{}"
minors:
  - {minor: "1.17", version: 1.17.18, kubernetes: ">= 1.29.0-0, < 1.33.0-0"}
  - {minor: "1.18", version: 1.18.17, kubernetes: ">= 1.30.0-0, < 1.34.0-0"}
  - {minor: "1.19", version: 1.19.11, kubernetes: ">= 1.32.0-0, < 1.36.0-0"}
`

var cluster = clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "default"}}

func handlers(t *testing.T, objs ...client.Object) *Handlers {
	t.Helper()
	g := NewWithT(t)
	c, err := catalog.Parse([]byte(testCatalog))
	g.Expect(err).ToNot(HaveOccurred())
	scheme := runtime.NewScheme()
	g.Expect(cniv1.AddToScheme(scheme)).To(Succeed())
	return &Handlers{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Catalogs: catalog.Catalogs{"cilium": c},
	}
}

func clusterCNI(version, observed, target string, ready bool) *cniv1.ClusterCNI {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	return &cniv1.ClusterCNI{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace},
		Spec:       cniv1.ClusterCNISpec{Type: "cilium", Version: version},
		Status: cniv1.ClusterCNIStatus{
			ObservedVersion: observed,
			TargetVersion:   target,
			Conditions:      []metav1.Condition{{Type: cniv1.ReadyCondition, Status: status, Reason: "Test"}},
		},
	}
}

func steps(versions ...string) []runtimehooksv1.UpgradeStepInfo {
	out := make([]runtimehooksv1.UpgradeStepInfo, 0, len(versions))
	for _, v := range versions {
		out = append(out, runtimehooksv1.UpgradeStepInfo{Version: v})
	}
	return out
}

func TestBeforeClusterUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		cni        *cniv1.ClusterCNI
		from       string
		steps      []string
		wantStatus runtimehooksv1.ResponseStatus
		wantRetry  bool
	}{
		{name: "no ClusterCNI", from: "v1.31.0", steps: []string{"v1.32.0"}, wantStatus: runtimehooksv1.ResponseStatusSuccess},
		{
			name: "chained upgrade the CNI can follow",
			cni:  clusterCNI("", "1.17.18", "1.17.18", true),
			from: "v1.31.0", steps: []string{"v1.32.0", "v1.33.0", "v1.34.0"},
			wantStatus: runtimehooksv1.ResponseStatusSuccess,
		},
		{
			name: "a step no CNI version supports",
			cni:  clusterCNI("", "1.17.18", "1.17.18", true),
			from: "v1.31.0", steps: []string{"v1.32.0", "v1.36.0"},
			wantStatus: runtimehooksv1.ResponseStatusFailure,
		},
		{
			name: "a pinned version that stops supporting the cluster",
			cni:  clusterCNI("1.17.18", "1.17.18", "1.17.18", true),
			from: "v1.31.0", steps: []string{"v1.32.0", "v1.33.0"},
			wantStatus: runtimehooksv1.ResponseStatusFailure,
		},
		{
			name: "a CNI rollout in progress",
			cni:  clusterCNI("", "1.17.18", "1.18.17", false),
			from: "v1.31.0", steps: []string{"v1.32.0"},
			wantStatus: runtimehooksv1.ResponseStatusSuccess, wantRetry: true,
		},
		{
			name: "a CNI that is not installed yet",
			cni:  clusterCNI("", "", "1.19.11", false),
			from: "v1.31.0", steps: []string{"v1.32.0"},
			wantStatus: runtimehooksv1.ResponseStatusSuccess, wantRetry: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			var objs []client.Object
			if tt.cni != nil {
				objs = append(objs, tt.cni)
			}
			req := &runtimehooksv1.BeforeClusterUpgradeRequest{
				Cluster: cluster, FromKubernetesVersion: tt.from,
				ToKubernetesVersion: tt.steps[len(tt.steps)-1], ControlPlaneUpgrades: steps(tt.steps...),
			}
			resp := &runtimehooksv1.BeforeClusterUpgradeResponse{}
			handlers(t, objs...).BeforeClusterUpgrade(context.Background(), req, resp)
			g.Expect(resp.Status).To(Equal(tt.wantStatus), resp.Message)
			if tt.wantRetry {
				g.Expect(resp.RetryAfterSeconds).ToNot(BeZero(), resp.Message)
			} else {
				g.Expect(resp.RetryAfterSeconds).To(BeZero(), resp.Message)
			}
		})
	}
}

func TestBeforeControlPlaneUpgrade(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	h := handlers(t, clusterCNI("", "1.18.17", "1.18.17", true))
	req := &runtimehooksv1.BeforeControlPlaneUpgradeRequest{Cluster: cluster, FromKubernetesVersion: "v1.33.0", ToKubernetesVersion: "v1.34.0"}

	// The first call announces the step and waits.
	resp := &runtimehooksv1.BeforeControlPlaneUpgradeResponse{}
	h.BeforeControlPlaneUpgrade(ctx, req, resp)
	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.RetryAfterSeconds).ToNot(BeZero())
	cni := &cniv1.ClusterCNI{}
	g.Expect(h.Client.Get(ctx, client.ObjectKeyFromObject(&cluster), cni)).To(Succeed())
	g.Expect(cni.Annotations).To(HaveKeyWithValue(cniv1.NextKubernetesVersionAnnotation, "v1.34.0"))

	// Cilium 1.18 does not support Kubernetes 1.34, so the control plane waits.
	resp = &runtimehooksv1.BeforeControlPlaneUpgradeResponse{}
	h.BeforeControlPlaneUpgrade(ctx, req, resp)
	g.Expect(resp.RetryAfterSeconds).ToNot(BeZero())

	// Once the controller moved Cilium to 1.19, the step proceeds.
	cni.Status.ObservedVersion, cni.Status.TargetVersion = "1.19.11", "1.19.11"
	g.Expect(h.Client.Update(ctx, cni)).To(Succeed())
	resp = &runtimehooksv1.BeforeControlPlaneUpgradeResponse{}
	h.BeforeControlPlaneUpgrade(ctx, req, resp)
	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.RetryAfterSeconds).To(BeZero(), resp.Message)
}

func TestBeforeWorkersUpgrade(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	req := &runtimehooksv1.BeforeWorkersUpgradeRequest{Cluster: cluster, FromKubernetesVersion: "v1.33.0", ToKubernetesVersion: "v1.34.0"}

	resp := &runtimehooksv1.BeforeWorkersUpgradeResponse{}
	handlers(t, clusterCNI("", "1.18.17", "1.19.11", false)).BeforeWorkersUpgrade(ctx, req, resp)
	g.Expect(resp.RetryAfterSeconds).ToNot(BeZero(), "workers wait for the CNI rollout")

	resp = &runtimehooksv1.BeforeWorkersUpgradeResponse{}
	handlers(t, clusterCNI("", "1.19.11", "1.19.11", true)).BeforeWorkersUpgrade(ctx, req, resp)
	g.Expect(resp.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))
	g.Expect(resp.RetryAfterSeconds).To(BeZero())
}
