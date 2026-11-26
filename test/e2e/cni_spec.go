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
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1 "sigs.k8s.io/cluster-api-addon-provider-helm/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
)

// cniSpec holds the state of one spec: its namespace and workload cluster.
type cniSpec struct {
	name          string
	namespace     *corev1.Namespace
	cancelWatches context.CancelFunc
	cluster       *clusterv1.Cluster
	result        *clusterctl.ApplyClusterTemplateAndWaitResult
	catalogs      catalog.Catalogs
}

func newSpec(name string) *cniSpec {
	catalogs, err := catalog.Builtin()
	Expect(err).ToNot(HaveOccurred())
	s := &cniSpec{name: name, catalogs: catalogs}
	s.namespace, s.cancelWatches = framework.SetupSpecNamespace(ctx, name, bootstrapClusterProxy, artifactFolder, nil)
	return s
}

// cleanup dumps the spec's resources and logs and deletes its cluster.
func (s *cniSpec) cleanup() {
	// The framework records the Cluster as soon as it exists, so a spec that
	// fails while creating it still collects the workload cluster's logs.
	if s.cluster == nil && s.result != nil {
		s.cluster = s.result.Cluster
	}
	framework.DumpSpecResourcesAndCleanup(ctx, s.name, bootstrapClusterProxy, clusterctlConfigPath, artifactFolder,
		s.namespace, s.cancelWatches, s.cluster, e2eConfig.GetIntervals, skipCleanup)
}

func (s *cniSpec) intervals(key string) []any {
	return e2eConfig.GetIntervals(s.name, key)
}

func (s *cniSpec) client() client.Client {
	return bootstrapClusterProxy.GetClient()
}

func (s *cniSpec) workload() client.Client {
	return bootstrapClusterProxy.GetWorkloadCluster(ctx, s.namespace.Name, s.cluster.Name).GetClient()
}

// kubernetesVersion returns a Kubernetes version from the e2e config by minor,
// e.g. "1.33".
func kubernetesVersion(minor string) string {
	return e2eConfig.MustGetVariable("KUBERNETES_VERSION_" + strings.ReplaceAll(minor, ".", "_"))
}

// createCluster creates a workload cluster from a template flavor and waits
// until its nodes are ready, which needs a working CNI.
func (s *cniSpec) createCluster(flavor, kubernetes string, cniSpec map[string]any) {
	s.createNamedCluster(fmt.Sprintf("%s-%s", s.name, randomSuffix()), flavor, kubernetes, cniSpec)
}

func randomSuffix() string {
	return rand.String(6)
}

// createNamedCluster is createCluster with a given cluster name.
func (s *cniSpec) createNamedCluster(name, flavor, kubernetes string, cniSpec map[string]any) {
	vars := map[string]string{}
	if cniSpec != nil {
		vars["E2E_CLUSTERCNI_SPEC"] = toJSON(cniSpec)
	}
	result := &clusterctl.ApplyClusterTemplateAndWaitResult{}
	s.result = result
	clusterctl.ApplyClusterTemplateAndWait(ctx, clusterctl.ApplyClusterTemplateAndWaitInput{
		ClusterProxy: bootstrapClusterProxy,
		ConfigCluster: clusterctl.ConfigClusterInput{
			LogFolder:                artifactFolder + "/clusters/" + bootstrapClusterProxy.GetName(),
			ClusterctlConfigPath:     clusterctlConfigPath,
			KubeconfigPath:           bootstrapClusterProxy.GetKubeconfigPath(),
			InfrastructureProvider:   clusterctl.DefaultInfrastructureProvider,
			Flavor:                   flavor,
			Namespace:                s.namespace.Name,
			ClusterName:              name,
			KubernetesVersion:        kubernetes,
			ControlPlaneMachineCount: ptr.To[int64](1),
			WorkerMachineCount:       ptr.To[int64](1),
			ClusterctlVariables:      vars,
		},
		WaitForClusterIntervals:      s.intervals("wait-cluster"),
		WaitForControlPlaneIntervals: s.intervals("wait-control-plane"),
		WaitForMachineDeployments:    s.intervals("wait-worker-nodes"),
	}, result)
	s.cluster = result.Cluster
}

// getCNI returns the cluster's ClusterCNI.
func (s *cniSpec) getCNI(g Gomega) *cniv1.ClusterCNI {
	cni := &cniv1.ClusterCNI{}
	g.Expect(s.client().Get(ctx, client.ObjectKeyFromObject(s.cluster), cni)).To(Succeed())
	return cni
}

// createCNI creates a ClusterCNI for a cluster made without one.
func (s *cniSpec) createCNI(spec cniv1.ClusterCNISpec) {
	Expect(s.client().Create(ctx, &cniv1.ClusterCNI{
		ObjectMeta: metav1.ObjectMeta{Name: s.cluster.Name, Namespace: s.namespace.Name},
		Spec:       spec,
	})).To(Succeed())
}

// updateCNI changes the ClusterCNI.
func (s *cniSpec) updateCNI(mutate func(*cniv1.ClusterCNI)) {
	Eventually(func(g Gomega) {
		cni := s.getCNI(g)
		mutate(cni)
		g.Expect(s.client().Update(ctx, cni)).To(Succeed())
	}, s.intervals("wait-cluster")...).Should(Succeed())
}

// expectCNI waits until the CNI runs version and returns every version the
// HelmReleaseProxy asked for on the way, in order.
func (s *cniSpec) expectCNI(cniType string, version *semver.Version) []string {
	cat := s.catalogs[cniType]
	want := cat.Format(version)
	var steps []string
	Eventually(func(g Gomega) {
		hrp := &helmv1.HelmReleaseProxy{}
		if err := s.client().Get(ctx, client.ObjectKey{Namespace: s.namespace.Name, Name: s.cluster.Name + "-cni"}, hrp); err == nil {
			if len(steps) == 0 || steps[len(steps)-1] != hrp.Spec.Version {
				steps = append(steps, hrp.Spec.Version)
				GinkgoWriter.Printf("%s HelmReleaseProxy at %s\n", cniType, hrp.Spec.Version)
			}
		}
		cni := s.getCNI(g)
		g.Expect(cni.Status.ObservedVersion).To(Equal(want), "conditions: %v", cni.Status.Conditions)
		g.Expect(meta.IsStatusConditionTrue(cni.Status.Conditions, cniv1.ReadyCondition)).To(BeTrue(), "conditions: %v", cni.Status.Conditions)
	}, s.intervals("wait-cni")...).Should(Succeed())
	return steps
}

// expectSteps checks the HelmReleaseProxy went through the catalog's upgrade
// path from observed to target, in order.
func (s *cniSpec) expectSteps(cniType string, steps []string, observed, target *semver.Version) {
	cat := s.catalogs[cniType]
	path, err := cat.Path(observed, target)
	Expect(err).ToNot(HaveOccurred())
	want := make([]string, 0, 1+len(path))
	want = append(want, cat.Format(observed))
	for _, v := range path {
		want = append(want, cat.Format(v))
	}
	// Polling can miss a short step, but never reorders them.
	Expect(slices.Compact(steps)).To(HaveExactElements(subsequence(want, steps)...), "steps %v must follow the path %v", steps, want)
	Expect(steps[len(steps)-1]).To(Equal(want[len(want)-1]))
}

// subsequence returns the elements of want that occur in got, in want's order.
func subsequence(want, got []string) []any {
	var out []any
	for _, w := range want {
		if slices.Contains(got, w) {
			out = append(out, w)
		}
	}
	return out
}

// target returns the version the controller should pick for a ClusterCNI spec
// version on the given Kubernetes versions.
func (s *cniSpec) target(cniType, requested string, observed *semver.Version, kubernetes ...string) *semver.Version {
	vs := make([]*semver.Version, 0, len(kubernetes))
	for _, k := range kubernetes {
		vs = append(vs, semver.MustParse(k))
	}
	v, err := s.catalogs[cniType].Target(requested, observed, vs...)
	Expect(err).ToNot(HaveOccurred())
	return v
}

// expectNetwork checks pod networking and service routing in the workload
// cluster: every node is ready and a pod resolves the API server Service
// through cluster DNS.
func (s *cniSpec) expectNetwork() {
	workload := s.workload()
	Eventually(func(g Gomega) {
		nodes := &corev1.NodeList{}
		g.Expect(workload.List(ctx, nodes)).To(Succeed())
		g.Expect(nodes.Items).ToNot(BeEmpty())
		for _, n := range nodes.Items {
			ready := false
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					ready = true
				}
			}
			g.Expect(ready).To(BeTrue(), "node %s is not ready", n.Name)
		}
	}, s.intervals("wait-nodes-ready")...).Should(Succeed())

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "network-check-", Namespace: metav1.NamespaceDefault},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](10),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "nslookup",
						Image:   "registry.k8s.io/e2e-test-images/busybox:1.36.1-1",
						Command: []string{"nslookup", "kubernetes.default.svc." + e2eConfig.MustGetVariable("DOCKER_SERVICE_DOMAIN")},
					}},
				},
			},
		},
	}
	Expect(workload.Create(ctx, job)).To(Succeed())
	Eventually(func(g Gomega) {
		got := &batchv1.Job{}
		g.Expect(workload.Get(ctx, client.ObjectKeyFromObject(job), got)).To(Succeed())
		g.Expect(got.Status.Succeeded).To(BeNumerically(">=", 1), "the network check job has not succeeded yet: %+v", got.Status)
	}, s.intervals("wait-network")...).Should(Succeed())
	Expect(workload.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
}

// machinesAt reports whether every Machine of the cluster runs a kubelet of
// the given version.
func (s *cniSpec) machinesAt(g Gomega, version string) {
	machines := &clusterv1.MachineList{}
	g.Expect(s.client().List(ctx, machines, client.InNamespace(s.namespace.Name),
		client.MatchingLabels{clusterv1.ClusterNameLabel: s.cluster.Name})).To(Succeed())
	g.Expect(machines.Items).ToNot(BeEmpty())
	for _, m := range machines.Items {
		g.Expect(m.Status.NodeInfo).ToNot(BeNil(), "machine %s has no node yet", m.Name)
		g.Expect(m.Status.NodeInfo.KubeletVersion).To(Equal(version), "machine %s", m.Name)
	}
}

// controlPlaneVersions returns the kubelet versions of the control plane nodes.
func (s *cniSpec) controlPlaneVersions(g Gomega) []string {
	machines := &clusterv1.MachineList{}
	g.Expect(s.client().List(ctx, machines, client.InNamespace(s.namespace.Name), client.MatchingLabels{
		clusterv1.ClusterNameLabel: s.cluster.Name, clusterv1.MachineControlPlaneLabel: "",
	})).To(Succeed())
	var versions []string
	for _, m := range machines.Items {
		if m.Status.NodeInfo != nil {
			versions = append(versions, m.Status.NodeInfo.KubeletVersion)
		}
	}
	return versions
}

func toJSON(v any) string {
	data, err := json.Marshal(v)
	Expect(err).ToNot(HaveOccurred())
	return string(data)
}
