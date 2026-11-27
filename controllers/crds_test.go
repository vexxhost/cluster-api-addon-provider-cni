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
	"testing"

	"github.com/Masterminds/semver/v3"
	. "github.com/onsi/gomega"

	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
)

// TestCRDObjects checks the CRDs come from wherever each version keeps them.
// It downloads charts, so it is skipped with -short.
func TestCRDObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads charts")
	}
	catalogs, err := catalog.Builtin()
	NewWithT(t).Expect(err).ToNot(HaveOccurred())
	kube := semver.MustParse("v1.33.0")

	tests := []struct {
		cni, version, wantCRD string
	}{
		// Helm never upgrades a chart's crds/ directory; 3.30 added Goldmane.
		{cni: "calico", version: "v3.30.7", wantCRD: "goldmanes.operator.tigera.io"},
		// From 3.32 the CRDs are in a separate chart.
		{cni: "calico", version: "v3.33.1", wantCRD: "installations.operator.tigera.io"},
		// Cilium creates its CRDs at runtime.
		{cni: "cilium", version: "1.19.11"},
	}
	for _, tt := range tests {
		t.Run(tt.cni+"-"+tt.version, func(t *testing.T) {
			g := NewWithT(t)
			r := &ClusterCNIReconciler{}
			cat := catalogs[tt.cni]
			objs, err := r.crdObjects(cat.Chart.RepoURL, cat, semver.MustParse(tt.version), kube)
			g.Expect(err).ToNot(HaveOccurred())
			var names []string
			for _, o := range objs {
				g.Expect(o.GetKind()).To(Equal("CustomResourceDefinition"))
				names = append(names, o.GetName())
			}
			if tt.wantCRD == "" {
				g.Expect(names).To(BeEmpty())
				return
			}
			g.Expect(names).To(ContainElement(tt.wantCRD))
		})
	}
}
