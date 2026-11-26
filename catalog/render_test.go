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

package catalog

import (
	"fmt"
	"testing"

	"github.com/Masterminds/semver/v3"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/vexxhost/cluster-api-addon-provider-cni/internal/charts"
)

func renderInput(kubeProxyReplacement bool, mirror string) RenderInput {
	return RenderInput{
		Cluster:              map[string]any{"metadata": map[string]any{"name": "test"}},
		KubeProxyReplacement: kubeProxyReplacement,
		IPv4PodCIDRs:         []string{"10.100.0.0/16"},
		ServiceCIDRs:         []string{"10.96.0.0/12"},
		ImageRepository:      mirror,
		APIServerHost:        "10.0.0.10",
		APIServerPort:        6443,
	}
}

func nested(t *testing.T, values map[string]any, path ...string) any {
	t.Helper()
	got, found, err := unstructured.NestedFieldNoCopy(values, path...)
	NewWithT(t).Expect(err).ToNot(HaveOccurred())
	if !found {
		return nil
	}
	return got
}

func TestCiliumValues(t *testing.T) {
	g := NewWithT(t)
	catalogs, err := Builtin()
	g.Expect(err).ToNot(HaveOccurred())
	c := catalogs["cilium"]

	values, err := c.Values(semver.MustParse("1.19.11"), renderInput(false, ""), nil)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(nested(t, values, "ipam", "operator", "clusterPoolIPv4PodCIDRList")).To(Equal([]any{"10.100.0.0/16"}))
	g.Expect(values).ToNot(HaveKey("kubeProxyReplacement"), "kube-proxy replacement is left to the chart default")
	g.Expect(values).ToNot(HaveKey("image"), "images are left to the chart default")

	values, err = c.Values(semver.MustParse("1.19.11"), renderInput(true, "registry.example.com/mirror"), nil)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(nested(t, values, "kubeProxyReplacement")).To(BeTrue())
	g.Expect(nested(t, values, "k8sServiceHost")).To(Equal("10.0.0.10"))
	g.Expect(nested(t, values, "k8sServicePort")).To(BeEquivalentTo(6443))
	g.Expect(nested(t, values, "image", "repository")).To(Equal("registry.example.com/mirror/cilium"))
	g.Expect(nested(t, values, "operator", "image", "repository")).To(Equal("registry.example.com/mirror/operator"))
}

func TestCalicoValues(t *testing.T) {
	g := NewWithT(t)
	catalogs, err := Builtin()
	g.Expect(err).ToNot(HaveOccurred())
	c := catalogs["calico"]

	values, err := c.Values(semver.MustParse("v3.33.1"), renderInput(false, ""), nil)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(nested(t, values, "installation", "enabled")).To(BeTrue())
	g.Expect(nested(t, values, "installation", "calicoNetwork")).To(BeNil(), "IP pools are left to the operator")

	values, err = c.Values(semver.MustParse("v3.33.1"), renderInput(true, "registry.example.com/mirror/"), nil)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(nested(t, values, "installation", "registry")).To(Equal("registry.example.com/mirror/"))
	g.Expect(nested(t, values, "tigeraOperator", "registry")).To(Equal("registry.example.com/mirror"))
	g.Expect(nested(t, values, "installation", "calicoNetwork", "linuxDataplane")).To(Equal("BPF"))
	g.Expect(nested(t, values, "kubernetesServiceEndpoint", "port")).To(Equal("6443"))
}

func TestValuesOverlay(t *testing.T) {
	g := NewWithT(t)
	catalogs, err := Builtin()
	g.Expect(err).ToNot(HaveOccurred())

	overlay := map[string]any{
		"cni":             map[string]any{"chainingMode": "portmap"},
		"sessionAffinity": true,
		"ipam":            map[string]any{"operator": map[string]any{"clusterPoolIPv4PodCIDRList": []any{"10.200.0.0/16"}}},
	}
	values, err := catalogs["cilium"].Values(semver.MustParse("1.19.11"), renderInput(false, ""), overlay)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(nested(t, values, "cni", "chainingMode")).To(Equal("portmap"))
	g.Expect(nested(t, values, "sessionAffinity")).To(BeTrue())
	g.Expect(nested(t, values, "ipam", "operator", "clusterPoolIPv4PodCIDRList")).To(Equal([]any{"10.200.0.0/16"}), "the overlay wins")
	g.Expect(overlay).To(HaveLen(3), "the overlay is not modified")
}

// TestCatalogsRenderWithUpstreamCharts renders every built-in minor with its
// real chart, which validates the values against the chart's schema and
// templates. It downloads the charts, so it is skipped with -short.
func TestCatalogsRenderWithUpstreamCharts(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads charts")
	}
	catalogs, err := Builtin()
	NewWithT(t).Expect(err).ToNot(HaveOccurred())

	for _, c := range catalogs {
		repo, err := charts.LoadRepo(c.Chart.RepoURL)
		NewWithT(t).Expect(err).ToNot(HaveOccurred())

		for _, m := range c.Minors {
			t.Run(c.Type+"-"+m.Minor, func(t *testing.T) {
				g := NewWithT(t)
				ch, err := repo.Fetch(c.Chart.Name, m.Version)
				g.Expect(err).ToNot(HaveOccurred())

				kube := oldestSupported(c, m.version)
				g.Expect(kube).ToNot(BeEmpty(), "no Kubernetes version in range")

				if crd := c.CRDChartFor(m.version); crd != nil {
					crdChart, err := repo.Fetch(crd.Name, m.Version)
					g.Expect(err).ToNot(HaveOccurred(), "the CRD chart must exist for every version that needs it")
					manifest, err := charts.Render(crdChart, crd.Release, c.Release.Namespace, kube, nil)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(manifest).To(ContainSubstring("kind: CustomResourceDefinition"))
				}

				for _, in := range []RenderInput{renderInput(false, ""), renderInput(true, "registry.example.com/mirror")} {
					values, err := c.Values(m.version, in, nil)
					g.Expect(err).ToNot(HaveOccurred())
					manifest, err := charts.Render(ch, c.Release.Name, c.Release.Namespace, kube, values)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(manifest).ToNot(BeEmpty())
					if in.ImageRepository != "" && c.Type == "cilium" {
						g.Expect(manifest).To(ContainSubstring("registry.example.com/mirror/cilium:"), "the agent image uses the mirror")
					}
				}
			})
		}
	}
}

func oldestSupported(c *Catalog, version *semver.Version) string {
	for minor := 20; minor < 60; minor++ {
		kube := fmt.Sprintf("v1.%d.0", minor)
		if c.Supports(version, semver.MustParse(kube)) {
			return kube
		}
	}
	return ""
}
