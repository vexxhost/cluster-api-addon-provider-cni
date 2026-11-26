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

	"github.com/Masterminds/semver/v3"
	"github.com/distribution/reference"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/external"
	utilyaml "sigs.k8s.io/cluster-api/util/yaml"

	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
)

// detect returns the version of a CNI installed without this controller, or
// nil when there is none.
func (r *ClusterCNIReconciler) detect(ctx context.Context, key client.ObjectKey, cat *catalog.Catalog) (*semver.Version, error) {
	c, err := r.ClusterCache.GetUncachedClient(ctx, key)
	if err != nil {
		return nil, err
	}
	for _, d := range cat.Detect {
		var spec corev1.PodSpec
		objKey := client.ObjectKey{Namespace: d.Namespace, Name: d.Name}
		switch d.Kind {
		case "DaemonSet":
			ds := &appsv1.DaemonSet{}
			if err := c.Get(ctx, objKey, ds); err != nil {
				if ignoreNotFound(err) == nil {
					continue
				}
				return nil, err
			}
			spec = ds.Spec.Template.Spec
		case "Deployment":
			deploy := &appsv1.Deployment{}
			if err := c.Get(ctx, objKey, deploy); err != nil {
				if ignoreNotFound(err) == nil {
					continue
				}
				return nil, err
			}
			spec = deploy.Spec.Template.Spec
		}
		for _, container := range spec.Containers {
			if container.Name == d.Container {
				v, err := imageVersion(container.Image)
				if err != nil {
					return nil, fmt.Errorf("found %s %s/%s but cannot read its version: %w", d.Kind, d.Namespace, d.Name, err)
				}
				return v, nil
			}
		}
	}
	return nil, nil
}

// imageVersion returns the version in an image tag, such as v1.15.3 in
// quay.io/cilium/cilium:v1.15.3@sha256:...
func imageVersion(image string) (*semver.Version, error) {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return nil, err
	}
	tagged, ok := ref.(reference.Tagged)
	if !ok {
		return nil, fmt.Errorf("image %s has no tag", image)
	}
	return semver.NewVersion(tagged.Tag())
}

// desiredKubernetesVersion returns the Kubernetes version the cluster moves
// to: the topology version for a ClusterClass cluster, otherwise the control
// plane's spec.version. It returns nil when neither is set.
func (r *ClusterCNIReconciler) desiredKubernetesVersion(ctx context.Context, cluster *clusterv1.Cluster) *semver.Version {
	version := cluster.Spec.Topology.Version
	if version == "" && cluster.Spec.ControlPlaneRef.IsDefined() {
		obj, err := external.GetObjectFromContractVersionedRef(ctx, r.APIReader, cluster.Spec.ControlPlaneRef, cluster.Namespace)
		if err != nil {
			ctrl.LoggerFrom(ctx).V(4).Info("Cannot read the control plane version", "error", err.Error())
			return nil
		}
		version, _, _ = unstructured.NestedString(obj.Object, "spec", "version")
	}
	if version == "" {
		return nil
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return nil
	}
	return v
}

// conflictingClusterResourceSet returns the name of a ClusterResourceSet with
// the Reconcile strategy that selects the Cluster and applies one of the
// catalog's detect targets. ClusterResourceSets that apply other add-ons, or
// only apply once, don't conflict.
func (r *ClusterCNIReconciler) conflictingClusterResourceSet(ctx context.Context, cluster *clusterv1.Cluster, cat *catalog.Catalog) (string, error) {
	sets := &addonsv1.ClusterResourceSetList{}
	if err := r.Client.List(ctx, sets, client.InNamespace(cluster.Namespace)); err != nil {
		return "", err
	}
	for _, crs := range sets.Items {
		if crs.Spec.Strategy != string(addonsv1.ClusterResourceSetStrategyReconcile) {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(&crs.Spec.ClusterSelector)
		if err != nil || selector.Empty() || !selector.Matches(labels.Set(cluster.Labels)) {
			continue
		}
		for _, res := range crs.Spec.Resources {
			contents, err := r.resourceSetContents(ctx, cluster.Namespace, res)
			if err != nil {
				return "", err
			}
			for _, content := range contents {
				objs, err := utilyaml.ToUnstructured(content)
				if err != nil {
					continue
				}
				for _, obj := range objs {
					for _, d := range cat.Detect {
						if obj.GetKind() == d.Kind && obj.GetName() == d.Name && obj.GetNamespace() == d.Namespace {
							return crs.Name, nil
						}
					}
				}
			}
		}
	}
	return "", nil
}

func (r *ClusterCNIReconciler) resourceSetContents(ctx context.Context, namespace string, res addonsv1.ResourceRef) ([][]byte, error) {
	key := client.ObjectKey{Namespace: namespace, Name: res.Name}
	var contents [][]byte
	switch res.Kind {
	case string(addonsv1.SecretClusterResourceSetResourceKind):
		s := &corev1.Secret{}
		if err := r.APIReader.Get(ctx, key, s); err != nil {
			return nil, ignoreNotFound(err)
		}
		for _, v := range s.Data {
			contents = append(contents, v)
		}
	case string(addonsv1.ConfigMapClusterResourceSetResourceKind):
		cm := &corev1.ConfigMap{}
		if err := r.APIReader.Get(ctx, key, cm); err != nil {
			return nil, ignoreNotFound(err)
		}
		for _, v := range cm.Data {
			contents = append(contents, []byte(v))
		}
	}
	return contents, nil
}
