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
	"strings"

	"github.com/Masterminds/semver/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	utilyaml "sigs.k8s.io/cluster-api/util/yaml"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
	"github.com/vexxhost/cluster-api-addon-provider-cni/internal/charts"
)

// fieldOwner is the server-side apply field manager in workload clusters.
const fieldOwner = "cluster-api-addon-provider-cni"

// applyCRDs installs the CRDs a CNI version needs before Helm installs or
// upgrades its chart: the chart's crds/ directory, which Helm only installs on
// the first install and never upgrades, and the catalog's CRD chart, if the
// version needs one. Both are server-side applied, the way upstream documents
// it for charts whose CRDs don't fit in a Helm release.
func (r *ClusterCNIReconciler) applyCRDs(ctx context.Context, key client.ObjectKey, cni *cniv1.ClusterCNI, cat *catalog.Catalog, version, kubernetes *semver.Version) error {
	repoURL := cat.Chart.RepoURL
	if cni.Spec.ChartRepository != "" {
		repoURL = cni.Spec.ChartRepository
	}
	objs, err := r.crdObjects(repoURL, cat, version, kubernetes)
	if err != nil {
		return err
	}
	if len(objs) == 0 {
		return nil
	}
	c, err := r.ClusterCache.GetClient(ctx, key)
	if err != nil {
		return err
	}
	for i := range objs {
		obj := objs[i].DeepCopy()
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("applying %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

// crdObjects returns the CRDs of a CNI version, caching them since released
// charts never change.
func (r *ClusterCNIReconciler) crdObjects(repoURL string, cat *catalog.Catalog, version, kubernetes *semver.Version) ([]unstructured.Unstructured, error) {
	formatted := cat.Format(version)
	cacheKey := strings.Join([]string{repoURL, cat.Chart.Name, formatted, fmt.Sprintf("%d.%d", kubernetes.Major(), kubernetes.Minor())}, "|")
	if objs, ok := r.crdCache.Load(cacheKey); ok {
		return objs.([]unstructured.Unstructured), nil
	}

	ch, err := charts.FetchChart(repoURL, cat.Chart.Name, formatted)
	if err != nil {
		return nil, err
	}
	var objs []unstructured.Unstructured
	for _, crd := range ch.CRDObjects() {
		parsed, err := utilyaml.ToUnstructured(crd.File.Data)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %s: %w", cat.Chart.Name, formatted, crd.Name, err)
		}
		objs = append(objs, parsed...)
	}

	if crd := cat.CRDChartFor(version); crd != nil {
		crdChart, err := charts.FetchChart(repoURL, crd.Name, formatted)
		if err != nil {
			return nil, err
		}
		manifest, err := charts.Render(crdChart, crd.Release, cat.Release.Namespace, "v"+release(kubernetes).String(), nil)
		if err != nil {
			return nil, fmt.Errorf("rendering %s %s: %w", crd.Name, formatted, err)
		}
		parsed, err := utilyaml.ToUnstructured([]byte(manifest))
		if err != nil {
			return nil, err
		}
		objs = append(objs, parsed...)
	}

	r.crdCache.Store(cacheKey, objs)
	return objs, nil
}

// ensureNamespace creates the release namespace with the catalog's labels,
// for example the Pod Security level a CNI needs. Helm would create it
// without labels.
func (r *ClusterCNIReconciler) ensureNamespace(ctx context.Context, key client.ObjectKey, rel catalog.Release) error {
	if len(rel.NamespaceLabels) == 0 {
		return nil
	}
	c, err := r.ClusterCache.GetUncachedClient(ctx, key)
	if err != nil {
		return err
	}
	ns := &corev1.Namespace{}
	ns.Name = rel.Namespace
	_, err = controllerutil.CreateOrPatch(ctx, c, ns, func() error {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		for k, v := range rel.NamespaceLabels {
			ns.Labels[k] = v
		}
		return nil
	})
	return err
}
