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

// applyCRDs installs the CRDs a CNI version needs from the catalog's CRD
// chart, the way upstream documents it: rendered with Helm and server-side
// applied. A Helm release can't hold them, because its revision would exceed
// the size limit of the Secret Helm stores it in.
func (r *ClusterCNIReconciler) applyCRDs(ctx context.Context, key client.ObjectKey, cni *cniv1.ClusterCNI, cat *catalog.Catalog, crd *catalog.CRDChart, version, kubernetes *semver.Version) error {
	repoURL := cat.Chart.RepoURL
	if cni.Spec.ChartRepository != "" {
		repoURL = cni.Spec.ChartRepository
	}
	objs, err := r.crdObjects(repoURL, crd, cat.Release.Namespace, cat.Format(version), kubernetes)
	if err != nil {
		return err
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

// crdObjects renders a CRD chart, caching the result since released charts
// never change.
func (r *ClusterCNIReconciler) crdObjects(repoURL string, crd *catalog.CRDChart, namespace, version string, kubernetes *semver.Version) ([]unstructured.Unstructured, error) {
	cacheKey := strings.Join([]string{repoURL, crd.Name, version, fmt.Sprintf("%d.%d", kubernetes.Major(), kubernetes.Minor())}, "|")
	if objs, ok := r.crdCache.Load(cacheKey); ok {
		return objs.([]unstructured.Unstructured), nil
	}
	ch, err := charts.FetchChart(repoURL, crd.Name, version)
	if err != nil {
		return nil, err
	}
	manifest, err := charts.Render(ch, crd.Release, namespace, "v"+release(kubernetes).String(), nil)
	if err != nil {
		return nil, fmt.Errorf("rendering %s %s: %w", crd.Name, version, err)
	}
	objs, err := utilyaml.ToUnstructured([]byte(manifest))
	if err != nil {
		return nil, err
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
