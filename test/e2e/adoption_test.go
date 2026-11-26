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
	"fmt"
	"io"
	"net/http"

	"github.com/Masterminds/semver/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
	"github.com/vexxhost/cluster-api-addon-provider-cni/internal/charts"
)

// The adoption specs reproduce how magnum-cluster-api installs a CNI: the
// manifests go into one Secret of a ClusterResourceSet with the ApplyOnce
// strategy, together with other add-ons, and a separate ClusterResourceSet
// with the Reconcile strategy applies yet another add-on to the same cluster.

// magnumCiliumValues are the values magnum-cluster-api renders Cilium with.
func magnumCiliumValues(podCIDR string) map[string]any {
	return map[string]any{
		"cni":             map[string]any{"chainingMode": "portmap"},
		"ipam":            map[string]any{"operator": map[string]any{"clusterPoolIPv4PodCIDRList": []any{podCIDR}}},
		"k8s":             map[string]any{"serviceProxyName": "cilium"},
		"sessionAffinity": true,
	}
}

// otherAddon is an unrelated manifest that shares the ClusterResourceSets.
const otherAddon = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cloud-config\n  namespace: kube-system\ndata:\n  cloud.conf: \"\"\n"

func (s *cniSpec) createClusterResourceSets(clusterName, cniFile, cniManifest string) {
	shared := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: s.namespace.Name},
		Type:       addonsv1.ClusterResourceSetSecretType,
		StringData: map[string]string{cniFile: cniManifest, "cloud-config.yaml": otherAddon},
	}
	other := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-other", Namespace: s.namespace.Name},
		Type:       addonsv1.ClusterResourceSetSecretType,
		StringData: map[string]string{"other.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: other-addon\n  namespace: kube-system\n"},
	}
	selector := metav1.LabelSelector{MatchLabels: map[string]string{"cluster-uuid": clusterName}}
	for _, obj := range []client.Object{
		shared, other,
		&addonsv1.ClusterResourceSet{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: s.namespace.Name},
			Spec: addonsv1.ClusterResourceSetSpec{
				ClusterSelector: selector,
				Resources:       []addonsv1.ResourceRef{{Kind: string(addonsv1.SecretClusterResourceSetResourceKind), Name: shared.Name}},
				Strategy:        string(addonsv1.ClusterResourceSetStrategyApplyOnce),
			},
		},
		&addonsv1.ClusterResourceSet{
			ObjectMeta: metav1.ObjectMeta{Name: other.Name, Namespace: s.namespace.Name},
			Spec: addonsv1.ClusterResourceSetSpec{
				ClusterSelector: selector,
				Resources:       []addonsv1.ResourceRef{{Kind: string(addonsv1.SecretClusterResourceSetResourceKind), Name: other.Name}},
				Strategy:        string(addonsv1.ClusterResourceSetStrategyReconcile),
			},
		},
	} {
		Expect(s.client().Create(ctx, obj)).To(Succeed())
	}
}

// createAdoptionCluster creates the ClusterResourceSets and a cluster they
// select. The cluster name must be known before the cluster exists, so the
// spec picks it here.
func (s *cniSpec) createAdoptionCluster(kubernetes, cniFile, cniManifest string) {
	name := fmt.Sprintf("%s-%s", s.name, randomSuffix())
	s.createClusterResourceSets(name, cniFile, cniManifest)
	s.createNamedCluster(name, "topology-adoption", kubernetes, nil)
}

type podState struct {
	generation int64
	pods       map[types.UID]bool
}

// daemonSetState records a DaemonSet's generation and pods, to check a
// takeover changes neither.
func (s *cniSpec) daemonSetState(namespace, name string) podState {
	workload := s.workload()
	ds := &appsv1.DaemonSet{}
	Expect(workload.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, ds)).To(Succeed())
	pods := &corev1.PodList{}
	Expect(workload.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels(ds.Spec.Selector.MatchLabels))).To(Succeed())
	state := podState{generation: ds.Generation, pods: map[types.UID]bool{}}
	for _, p := range pods.Items {
		state.pods[p.UID] = true
	}
	return state
}

var _ = Describe("Adopting a magnum-cluster-api shaped Cilium [Adoption-Cilium]", func() {
	var s *cniSpec
	BeforeEach(func() { s = newSpec("adoption-cilium") })
	AfterEach(func() { s.cleanup() })

	It("takes over Cilium 1.15.3 without restarting it, then upgrades one minor at a time", func() {
		// Like real clusters, Cilium 1.15 runs on a Kubernetes version newer
		// than the ones it was tested with.
		k8s := kubernetesVersion("1.32")
		podCIDR := e2eConfig.MustGetVariable("DOCKER_POD_CIDRS")
		running := semver.MustParse("1.15.3")

		By("Rendering Cilium 1.15.3 the way magnum-cluster-api does")
		repo, err := charts.LoadRepo("https://helm.cilium.io")
		Expect(err).ToNot(HaveOccurred())
		chart, err := repo.Fetch("cilium", "1.15.3")
		Expect(err).ToNot(HaveOccurred())
		manifest, err := charts.Render(chart, "cilium", "kube-system", "", magnumCiliumValues(podCIDR))
		Expect(err).ToNot(HaveOccurred())

		By("Creating a cluster that gets Cilium from a ClusterResourceSet")
		s.createAdoptionCluster(k8s, "cilium.yaml", manifest)
		s.expectNetwork()
		before := s.daemonSetState("kube-system", "cilium")

		By("Handing Cilium over to a ClusterCNI at its running version")
		s.createCNI(cniv1.ClusterCNISpec{
			Type:    "cilium",
			Version: running.String(),
			ProviderConfig: providerConfig(map[string]any{
				"cni":             map[string]any{"chainingMode": "portmap"},
				"k8s":             map[string]any{"serviceProxyName": "cilium"},
				"sessionAffinity": true,
			}),
		})
		s.expectCNI("cilium", running)

		By("Checking the takeover changed neither the agent DaemonSet nor its pods")
		after := s.daemonSetState("kube-system", "cilium")
		Expect(after.generation).To(Equal(before.generation))
		Expect(after.pods).To(Equal(before.pods))
		s.expectNetwork()

		By("Unpinning the version")
		newest := s.target("cilium", "", running, k8s)
		s.updateCNI(func(c *cniv1.ClusterCNI) { c.Spec.Version = "" })
		steps := s.expectCNI("cilium", newest)
		s.expectSteps("cilium", steps, running, newest)
		s.expectNetwork()
	})
})

var _ = Describe("Adopting a magnum-cluster-api shaped Calico [Adoption-Calico]", func() {
	var s *cniSpec
	BeforeEach(func() { s = newSpec("adoption-calico") })
	AfterEach(func() { s.cleanup() })

	It("takes over the Calico v3.32.1 manifests through the operator, then upgrades", func() {
		k8s := kubernetesVersion("1.35")
		running := semver.MustParse("3.32.1")

		By("Downloading the upstream Calico v3.32.1 manifest magnum-cluster-api ships")
		resp, err := http.Get("https://raw.githubusercontent.com/projectcalico/calico/v3.32.1/manifests/calico.yaml") //nolint:noctx
		Expect(err).ToNot(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		manifest, err := io.ReadAll(resp.Body)
		Expect(err).ToNot(HaveOccurred())

		By("Creating a cluster that gets Calico from a ClusterResourceSet")
		s.createAdoptionCluster(k8s, "calico.yml", string(manifest))
		s.expectNetwork()

		By("Handing Calico over to a ClusterCNI at its running version")
		s.createCNI(cniv1.ClusterCNISpec{Type: "calico", Version: running.String()})
		s.expectCNI("calico", running)

		By("Waiting for the operator to move Calico from kube-system to calico-system")
		workload := s.workload()
		Eventually(func(g Gomega) {
			ds := &appsv1.DaemonSet{}
			g.Expect(workload.Get(ctx, client.ObjectKey{Namespace: "calico-system", Name: "calico-node"}, ds)).To(Succeed())
			g.Expect(ds.Status.DesiredNumberScheduled).To(BeNumerically(">", 0))
			g.Expect(ds.Status.NumberReady).To(Equal(ds.Status.DesiredNumberScheduled))
			err := workload.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "calico-node"}, &appsv1.DaemonSet{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the manifest-based calico-node is still there")
		}, s.intervals("wait-cni")...).Should(Succeed())
		s.expectNetwork()

		By("Unpinning the version")
		newest := s.target("calico", "", running, k8s)
		s.updateCNI(func(c *cniv1.ClusterCNI) { c.Spec.Version = "" })
		steps := s.expectCNI("calico", newest)
		s.expectSteps("calico", steps, running, newest)
		s.expectNetwork()
	})
})

func providerConfig(values map[string]any) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(toJSON(values))}
}
