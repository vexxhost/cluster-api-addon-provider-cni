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
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	helmv1 "sigs.k8s.io/cluster-api-addon-provider-helm/api/v1alpha1"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/kubeconfig"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
)

// The workload cluster of every test Cluster is the envtest API server itself.

func newNamespace(ctx context.Context, t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cni-"}}
	NewWithT(t).Expect(envClient.Create(ctx, ns)).To(Succeed())
	t.Cleanup(func() { _ = envClient.Delete(context.Background(), ns) })
	return ns.Name
}

func newCluster(ctx context.Context, t *testing.T, namespace string, labels map[string]string) *clusterv1.Cluster {
	t.Helper()
	g := NewWithT(t)
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace, Labels: labels},
		Spec: clusterv1.ClusterSpec{
			ClusterNetwork: clusterv1.ClusterNetwork{
				Pods: clusterv1.NetworkRanges{CIDRBlocks: []string{"10.100.0.0/16"}},
			},
			ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: "10.0.0.10", Port: 6443},
		},
	}
	g.Expect(envClient.Create(ctx, cluster)).To(Succeed())
	g.Expect(envClient.Create(ctx, kubeconfig.GenerateSecret(cluster, kubeconfig.FromEnvTestConfig(envConfig, cluster)))).To(Succeed())
	cluster.Status.Initialization.InfrastructureProvisioned = ptr.To(true)
	cluster.Status.Initialization.ControlPlaneInitialized = ptr.To(true)
	g.Expect(envClient.Status().Update(ctx, cluster)).To(Succeed())
	return cluster
}

func newClusterCNI(ctx context.Context, t *testing.T, cluster *clusterv1.Cluster, spec cniv1.ClusterCNISpec) *cniv1.ClusterCNI {
	t.Helper()
	cni := &cniv1.ClusterCNI{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace},
		Spec:       spec,
	}
	NewWithT(t).Expect(envClient.Create(ctx, cni)).To(Succeed())
	return cni
}

func getHRP(ctx context.Context, g Gomega, cluster *clusterv1.Cluster) *helmv1.HelmReleaseProxy {
	hrp := &helmv1.HelmReleaseProxy{}
	g.Expect(envClient.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name + "-cni"}, hrp)).To(Succeed())
	return hrp
}

// markDeployed does what CAAPH does once a release is deployed.
func markDeployed(ctx context.Context, t *testing.T, cluster *clusterv1.Cluster, version string) {
	t.Helper()
	g := NewWithT(t)
	g.Eventually(func(g Gomega) {
		hrp := getHRP(ctx, g, cluster)
		g.Expect(hrp.Spec.Version).To(Equal(version))
		hrp.Status.ObservedGeneration = hrp.Generation
		hrp.Status.Status = "deployed"
		conditions.Set(hrp, metav1.Condition{Type: clusterv1.ReadyCondition, Status: metav1.ConditionTrue, Reason: "Deployed"})
		g.Expect(envClient.Status().Update(ctx, hrp)).To(Succeed())
	}, eventually...).Should(Succeed())
}

func readyReason(ctx context.Context, g Gomega, cni *cniv1.ClusterCNI) string {
	got := &cniv1.ClusterCNI{}
	g.Expect(envClient.Get(ctx, client.ObjectKeyFromObject(cni), got)).To(Succeed())
	c := meta.FindStatusCondition(got.Status.Conditions, cniv1.ReadyCondition)
	if c == nil {
		return ""
	}
	return c.Reason
}

func TestFreshInstall(t *testing.T) {
	requireEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()

	cluster := newCluster(ctx, t, newNamespace(ctx, t), nil)
	cni := newClusterCNI(ctx, t, cluster, cniv1.ClusterCNISpec{Type: "calico"})

	// Auto mode picks the newest Calico for the envtest Kubernetes version.
	g.Eventually(func(g Gomega) {
		hrp := getHRP(ctx, g, cluster)
		g.Expect(hrp.Spec.ChartName).To(Equal("tigera-operator"))
		g.Expect(hrp.Spec.ReleaseNamespace).To(Equal("tigera-operator"))
		g.Expect(hrp.Spec.Options.TakeOwnership).To(BeTrue())
		g.Expect(hrp.Annotations).ToNot(HaveKey(adoptingAnnotation))
		g.Expect(hrp.OwnerReferences).To(ContainElement(HaveField("Name", cluster.Name)))
		g.Expect(readyReason(ctx, g, cni)).To(Equal(cniv1.InstallingReason))
	}, eventually...).Should(Succeed())

	hrp := getHRP(ctx, g, cluster)
	markDeployed(ctx, t, cluster, hrp.Spec.Version)
	g.Eventually(func(g Gomega) {
		got := &cniv1.ClusterCNI{}
		g.Expect(envClient.Get(ctx, client.ObjectKeyFromObject(cni), got)).To(Succeed())
		g.Expect(got.Status.ObservedVersion).To(Equal(hrp.Spec.Version))
		g.Expect(got.Status.TargetVersion).To(Equal(hrp.Spec.Version))
		g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, cniv1.ReadyCondition)).To(BeTrue())
		g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, cniv1.UpgradeCompatibleCondition)).To(BeTrue())
		g.Expect(got.OwnerReferences).To(ContainElement(HaveField("Name", cluster.Name)))
	}, eventually...).Should(Succeed())

	// An announced Kubernetes version the cluster already runs is cleared.
	g.Expect(envClient.Get(ctx, client.ObjectKeyFromObject(cni), cni)).To(Succeed())
	cni.Annotations = map[string]string{cniv1.NextKubernetesVersionAnnotation: "v1.20.0"}
	g.Expect(envClient.Update(ctx, cni)).To(Succeed())
	g.Eventually(func(g Gomega) {
		got := &cniv1.ClusterCNI{}
		g.Expect(envClient.Get(ctx, client.ObjectKeyFromObject(cni), got)).To(Succeed())
		g.Expect(got.Annotations).ToNot(HaveKey(cniv1.NextKubernetesVersionAnnotation))
	}, eventually...).Should(Succeed())
}

func TestAdoptAndUpgrade(t *testing.T) {
	requireEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()

	// Cilium installed without Helm, the way a ClusterResourceSet would.
	ds := ciliumDaemonSet("quay.io/cilium/cilium:v1.15.3@sha256:" + sha256Zeros)
	g.Expect(envClient.Create(ctx, ds)).To(Succeed())
	t.Cleanup(func() { _ = envClient.Delete(context.Background(), ds) })

	namespace := newNamespace(ctx, t)
	cluster := newCluster(ctx, t, namespace, map[string]string{"cluster-uuid": "1234"})
	// A ClusterResourceSet that reconciles another add-on doesn't conflict.
	createClusterResourceSet(ctx, t, namespace, "other-addon", addonsv1.ClusterResourceSetStrategyReconcile, configMapManifest())
	cni := newClusterCNI(ctx, t, cluster, cniv1.ClusterCNISpec{
		Type:           "cilium",
		Version:        "1.17",
		ProviderConfig: providerConfig(t, map[string]any{"cni": map[string]any{"chainingMode": "portmap"}}),
	})

	// The takeover installs the running version first.
	g.Eventually(func(g Gomega) {
		hrp := getHRP(ctx, g, cluster)
		g.Expect(hrp.Spec.Version).To(Equal("1.15.3"))
		g.Expect(hrp.Annotations).To(HaveKeyWithValue(adoptingAnnotation, "true"))
		values := map[string]any{}
		g.Expect(yaml.Unmarshal([]byte(hrp.Spec.Values), &values)).To(Succeed())
		g.Expect(values).To(HaveKeyWithValue("cni", map[string]any{"chainingMode": "portmap"}))
		g.Expect(readyReason(ctx, g, cni)).To(Equal(cniv1.AdoptingReason))
	}, eventually...).Should(Succeed())

	// Then it steps to the latest patch and one minor at a time.
	for _, step := range []string{"1.15.3", "1.15.19", "1.16.19"} {
		markDeployed(ctx, t, cluster, step)
		g.Eventually(func(g Gomega) {
			g.Expect(readyReason(ctx, g, cni)).To(Equal(cniv1.UpgradingReason))
		}, eventually...).Should(Succeed())
	}
	g.Expect(getHRP(ctx, g, cluster).Annotations).ToNot(HaveKey(adoptingAnnotation))
	markDeployed(ctx, t, cluster, "1.17.18")
	g.Eventually(func(g Gomega) {
		got := &cniv1.ClusterCNI{}
		g.Expect(envClient.Get(ctx, client.ObjectKeyFromObject(cni), got)).To(Succeed())
		g.Expect(got.Status.ObservedVersion).To(Equal("1.17.18"))
		g.Expect(readyReason(ctx, g, cni)).To(Equal(cniv1.ReadyReason))
		// Cilium 1.17 is not tested with the envtest Kubernetes version.
		g.Expect(meta.IsStatusConditionFalse(got.Status.Conditions, cniv1.UpgradeCompatibleCondition)).To(BeTrue())
	}, eventually...).Should(Succeed())

	// A downgrade is refused and leaves the release alone.
	g.Expect(envClient.Get(ctx, client.ObjectKeyFromObject(cni), cni)).To(Succeed())
	cni.Spec.Version = "1.16"
	g.Expect(envClient.Update(ctx, cni)).To(Succeed())
	g.Eventually(func(g Gomega) {
		g.Expect(readyReason(ctx, g, cni)).To(Equal(cniv1.DowngradeRejectedReason))
	}, eventually...).Should(Succeed())
	g.Expect(getHRP(ctx, g, cluster).Spec.Version).To(Equal("1.17.18"))
}

func TestConflictingClusterResourceSet(t *testing.T) {
	requireEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()

	namespace := newNamespace(ctx, t)
	cluster := newCluster(ctx, t, namespace, map[string]string{"cluster-uuid": "5678"})
	manifest, err := yaml.Marshal(ciliumDaemonSet("quay.io/cilium/cilium:v1.15.3"))
	g.Expect(err).ToNot(HaveOccurred())
	createClusterResourceSet(ctx, t, namespace, "cilium", addonsv1.ClusterResourceSetStrategyReconcile, string(manifest))
	cni := newClusterCNI(ctx, t, cluster, cniv1.ClusterCNISpec{Type: "cilium", Version: "1.15.19"})

	g.Eventually(func(g Gomega) {
		g.Expect(readyReason(ctx, g, cni)).To(Equal(cniv1.ConflictingClusterResourceSetReason))
	}, eventually...).Should(Succeed())
	hrp := &helmv1.HelmReleaseProxy{}
	err = envClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cluster.Name + "-cni"}, hrp)
	g.Expect(client.IgnoreNotFound(err)).To(Succeed())
	g.Expect(err).To(HaveOccurred(), "no HelmReleaseProxy while a ClusterResourceSet keeps applying the CNI")
}

func TestImageVersion(t *testing.T) {
	tests := map[string]string{
		"quay.io/cilium/cilium:v1.15.3":                       "1.15.3",
		"quay.io/cilium/cilium:v1.15.3@sha256:" + sha256Zeros: "1.15.3",
		"registry.example.com/cilium-cilium:1.16.2":           "1.16.2",
		"docker.io/calico/node:v3.32.1":                       "3.32.1",
		"localhost:5000/mirror/calico/node:v3.32.1":           "3.32.1",
	}
	for image, want := range tests {
		t.Run(image, func(t *testing.T) {
			g := NewWithT(t)
			v, err := imageVersion(image)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(v.String()).To(Equal(want))
		})
	}
	_, err := imageVersion("quay.io/cilium/cilium@sha256:" + sha256Zeros)
	NewWithT(t).Expect(err).To(MatchError(ContainSubstring("no tag")))
}

const sha256Zeros = "0000000000000000000000000000000000000000000000000000000000000000"

func ciliumDaemonSet(image string) *appsv1.DaemonSet {
	labels := map[string]string{"k8s-app": "cilium"}
	return &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "cilium", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "cilium-agent", Image: image}}},
			},
		},
	}
}

func configMapManifest() string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cloud-config\n  namespace: kube-system\n"
}

func createClusterResourceSet(ctx context.Context, t *testing.T, namespace, name string, strategy addonsv1.ClusterResourceSetStrategy, manifest string) {
	t.Helper()
	g := NewWithT(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       addonsv1.ClusterResourceSetSecretType,
		StringData: map[string]string{"manifest.yaml": manifest},
	}
	g.Expect(envClient.Create(ctx, secret)).To(Succeed())
	g.Expect(envClient.Create(ctx, &addonsv1.ClusterResourceSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: addonsv1.ClusterResourceSetSpec{
			ClusterSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "cluster-uuid", Operator: metav1.LabelSelectorOpExists},
			}},
			Resources: []addonsv1.ResourceRef{{Kind: string(addonsv1.SecretClusterResourceSetResourceKind), Name: name}},
			Strategy:  string(strategy),
		},
	})).To(Succeed())
}

func providerConfig(t *testing.T, values map[string]any) *apiextensionsv1.JSON {
	t.Helper()
	raw, err := json.Marshal(values)
	NewWithT(t).Expect(err).ToNot(HaveOccurred())
	return &apiextensionsv1.JSON{Raw: raw}
}
