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
	"os"

	"github.com/Masterminds/semver/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	controlplanev1 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/test/framework"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
)

var _ = Describe("Cilium on a ClusterClass cluster [Cilium]", func() {
	var s *cniSpec
	BeforeEach(func() { s = newSpec("cilium") })
	AfterEach(func() { s.cleanup() })

	It("installs a pinned minor, then follows the newest version one minor at a time", func() {
		k8s := kubernetesVersion("1.33")
		pinned := s.target("cilium", "1.18", nil)

		By("Creating a cluster with Cilium pinned to 1.18")
		s.createCluster("topology", k8s, map[string]any{"type": "cilium", "version": "1.18"})
		s.expectCNI("cilium", pinned)
		s.expectNetwork()

		By("Unpinning the version")
		newest := s.target("cilium", "", pinned, k8s)
		Expect(newest.GreaterThan(pinned)).To(BeTrue(), "the catalog should have a newer Cilium for Kubernetes %s", k8s)
		s.updateCNI(func(c *cniv1.ClusterCNI) { c.Spec.Version = "" })
		steps := s.expectCNI("cilium", newest)
		s.expectSteps("cilium", steps, pinned, newest)
		s.expectNetwork()
	})
})

var _ = Describe("Calico through a chained Kubernetes upgrade with lifecycle hooks [Calico-Chained]", func() {
	var s *cniSpec
	BeforeEach(func() { s = newSpec("calico-chained") })
	AfterEach(func() { s.cleanup() })

	It("installs Calico and keeps it ahead of every control plane step", func() {
		versions := []string{
			kubernetesVersion("1.32"), kubernetesVersion("1.33"), kubernetesVersion("1.34"),
			kubernetesVersion("1.35"), kubernetesVersion("1.36"),
		}
		from, to := versions[0], versions[len(versions)-1]
		installed := s.target("calico", "", nil, from)

		By("Creating a cluster with the newest Calico for Kubernetes " + from)
		s.createCluster("topology", from, map[string]any{"type": "calico"})
		s.expectCNI("calico", installed)
		s.expectNetwork()

		By("Allowing a chained upgrade through every minor in the ClusterClass")
		cc := &clusterv1.ClusterClass{}
		Expect(s.client().Get(ctx, client.ObjectKey{Namespace: s.namespace.Name, Name: s.cluster.Spec.Topology.ClassRef.Name}, cc)).To(Succeed())
		base := cc.DeepCopy()
		cc.Spec.KubernetesVersions = versions
		Expect(s.client().Patch(ctx, cc, client.MergeFrom(base))).To(Succeed())

		By("Upgrading the cluster from " + from + " to " + to + " in one step")
		cluster := &clusterv1.Cluster{}
		Expect(s.client().Get(ctx, client.ObjectKeyFromObject(s.cluster), cluster)).To(Succeed())
		base2 := cluster.DeepCopy()
		cluster.Spec.Topology.Version = to
		Expect(s.client().Patch(ctx, cluster, client.MergeFrom(base2))).To(Succeed())

		cat := s.catalogs["calico"]
		Eventually(func(g Gomega) {
			// At no point may a control plane node run a version the CNI does
			// not support: the hooks hold each step until it does.
			cni := s.getCNI(g)
			if cni.Status.ObservedVersion != "" {
				observed := semver.MustParse(cni.Status.ObservedVersion)
				for _, kubelet := range s.controlPlaneVersions(g) {
					if !cat.Supports(observed, semver.MustParse(kubelet)) {
						StopTrying("a control plane node runs " + kubelet + " while Calico runs " + cni.Status.ObservedVersion).Now()
					}
				}
			}
			s.machinesAt(g, to)
			g.Expect(meta.IsStatusConditionTrue(cni.Status.Conditions, cniv1.ReadyCondition)).To(BeTrue())
		}, s.intervals("wait-upgrade")...).Should(Succeed())

		upgraded := s.target("calico", "", installed, versions[len(versions)-2], to)
		s.expectCNI("calico", upgraded)
		Expect(s.getCNI(Default).Annotations).ToNot(HaveKey(cniv1.NextKubernetesVersionAnnotation))
		s.expectNetwork()
	})
})

var _ = Describe("Calico on a cluster without a ClusterClass [KCP]", func() {
	var s *cniSpec
	BeforeEach(func() { s = newSpec("kcp") })
	AfterEach(func() { s.cleanup() })

	It("installs Calico and follows a Kubernetes upgrade staged with the annotation", func() {
		from, to := kubernetesVersion("1.34"), kubernetesVersion("1.35")
		installed := s.target("calico", "", nil, from)

		By("Creating a KubeadmControlPlane cluster with Calico")
		s.createCluster("kcp", from, map[string]any{"type": "calico"})
		s.expectCNI("calico", installed)
		s.expectNetwork()

		By("Announcing Kubernetes " + to)
		staged := s.target("calico", "", installed, from, to)
		s.updateCNI(func(c *cniv1.ClusterCNI) {
			if c.Annotations == nil {
				c.Annotations = map[string]string{}
			}
			c.Annotations[cniv1.NextKubernetesVersionAnnotation] = to
		})
		s.expectCNI("calico", staged)

		By("Upgrading the control plane and workers to " + to)
		kcp := &controlplanev1.KubeadmControlPlane{}
		Expect(s.client().Get(ctx, client.ObjectKey{Namespace: s.namespace.Name, Name: s.cluster.Spec.ControlPlaneRef.Name}, kcp)).To(Succeed())
		base := kcp.DeepCopy()
		kcp.Spec.Version = to
		Expect(s.client().Patch(ctx, kcp, client.MergeFrom(base))).To(Succeed())
		framework.WaitForControlPlaneMachinesToBeUpgraded(ctx, framework.WaitForControlPlaneMachinesToBeUpgradedInput{
			Lister: s.client(), Cluster: s.cluster, MachineCount: 1, KubernetesUpgradeVersion: to,
		}, s.intervals("wait-upgrade")...)

		By("Checking the annotation is cleared and Calico follows the new version")
		Eventually(func(g Gomega) {
			g.Expect(s.getCNI(g).Annotations).ToNot(HaveKey(cniv1.NextKubernetesVersionAnnotation))
		}, s.intervals("wait-cni")...).Should(Succeed())
		s.expectCNI("calico", s.target("calico", "", staged, to))
		s.expectNetwork()
	})
})

// The catalog spec runs once per catalog minor changed in a pull request. The
// workflow sets E2E_CATALOG_CNI and E2E_CATALOG_MINOR.
var _ = Describe("A catalog minor [Catalog]", func() {
	var s *cniSpec
	BeforeEach(func() {
		if os.Getenv("E2E_CATALOG_CNI") == "" || os.Getenv("E2E_CATALOG_MINOR") == "" {
			Skip("E2E_CATALOG_CNI and E2E_CATALOG_MINOR are not set")
		}
		s = newSpec("catalog")
	})
	AfterEach(func() {
		if s != nil {
			s.cleanup()
		}
	})

	It("installs on the newest supported Kubernetes version and upgrades from the previous minor", func() {
		cniType, minor := os.Getenv("E2E_CATALOG_CNI"), os.Getenv("E2E_CATALOG_MINOR")
		cat := s.catalogs[cniType]
		Expect(cat).ToNot(BeNil(), "unknown CNI %s", cniType)
		target := s.target(cniType, minor, nil)

		var k8s string
		for _, m := range []string{"1.37", "1.36", "1.35", "1.34", "1.33", "1.32"} {
			if v := kubernetesVersion(m); cat.Supports(target, semver.MustParse(v)) {
				k8s = v
				break
			}
		}
		Expect(k8s).ToNot(BeEmpty(), "no Kubernetes version in the e2e config supports %s %s", cniType, minor)

		var previous *semver.Version
		for i, m := range cat.Minors {
			if m.Minor == minor && i > 0 {
				previous = semver.MustParse(cat.Minors[i-1].Version)
			}
		}
		if previous == nil {
			By("Installing " + cniType + " " + cat.Format(target) + " on Kubernetes " + k8s)
			s.createCluster("topology", k8s, map[string]any{"type": cniType, "version": target.String()})
			s.expectCNI(cniType, target)
			s.expectNetwork()
			return
		}

		By("Installing " + cniType + " " + cat.Format(previous) + " on Kubernetes " + k8s)
		s.createCluster("topology", k8s, map[string]any{"type": cniType, "version": previous.String()})
		s.expectCNI(cniType, previous)
		s.expectNetwork()

		By("Upgrading to " + cat.Format(target))
		s.updateCNI(func(c *cniv1.ClusterCNI) { c.Spec.Version = target.String() })
		steps := s.expectCNI(cniType, target)
		s.expectSteps(cniType, steps, previous, target)
		s.expectNetwork()
	})
})
