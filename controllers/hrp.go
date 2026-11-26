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
	"fmt"
	"net/netip"

	"github.com/Masterminds/semver/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	helmv1 "sigs.k8s.io/cluster-api-addon-provider-helm/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
)

// adoptingAnnotation marks a HelmReleaseProxy that takes over an existing CNI.
const adoptingAnnotation = "cni.vexxhost.com/adopting"

// helmReleaseProxyName is the name of the HelmReleaseProxy for a Cluster's CNI.
func helmReleaseProxyName(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-cni"
}

func (r *ClusterCNIReconciler) getHelmReleaseProxy(ctx context.Context, cluster *clusterv1.Cluster) (*helmv1.HelmReleaseProxy, error) {
	hrp := &helmv1.HelmReleaseProxy{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: helmReleaseProxyName(cluster)}, hrp)
	if err != nil {
		return nil, ignoreNotFound(err)
	}
	return hrp, nil
}

// helmReleaseProxyReady reports whether CAAPH deployed the current spec.
func helmReleaseProxyReady(hrp *helmv1.HelmReleaseProxy) bool {
	return hrp != nil &&
		hrp.Status.ObservedGeneration == hrp.Generation &&
		hrp.Status.Status == "deployed" &&
		conditions.IsTrue(hrp, clusterv1.ReadyCondition)
}

// releaseMessage returns the HelmReleaseProxy's failure message, if any.
func releaseMessage(hrp *helmv1.HelmReleaseProxy) string {
	c := conditions.Get(hrp, helmv1.HelmReleaseReadyCondition)
	if c == nil || c.Status != "False" || c.Message == "" || hrp.Status.ObservedGeneration != hrp.Generation {
		return ""
	}
	return ": " + c.Message
}

// applyHelmReleaseProxy creates or updates the HelmReleaseProxy for version.
// It is owned by the Cluster: deleting a HelmReleaseProxy uninstalls the
// release, so deleting the ClusterCNI must leave it alone.
func (r *ClusterCNIReconciler) applyHelmReleaseProxy(ctx context.Context, cluster *clusterv1.Cluster, cni *cniv1.ClusterCNI, cat *catalog.Catalog, version *semver.Version, adopting bool) (*helmv1.HelmReleaseProxy, error) {
	in, err := renderInput(cluster, cni, adopting)
	if err != nil {
		return nil, err
	}
	overlay := map[string]any{}
	if cni.Spec.ProviderConfig != nil && len(cni.Spec.ProviderConfig.Raw) > 0 {
		if err := json.Unmarshal(cni.Spec.ProviderConfig.Raw, &overlay); err != nil {
			return nil, fmt.Errorf("spec.providerConfig: %w", err)
		}
	}
	values, err := cat.Values(version, in, overlay)
	if err != nil {
		return nil, err
	}
	rendered, err := yaml.Marshal(values)
	if err != nil {
		return nil, err
	}

	hrp := &helmv1.HelmReleaseProxy{}
	hrp.Name = helmReleaseProxyName(cluster)
	hrp.Namespace = cluster.Namespace
	_, err = controllerutil.CreateOrPatch(ctx, r.Client, hrp, func() error {
		if hrp.Labels == nil {
			hrp.Labels = map[string]string{}
		}
		hrp.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		hrp.Labels[cniv1.ClusterCNINameLabel] = cni.Name
		if adopting {
			if hrp.Annotations == nil {
				hrp.Annotations = map[string]string{}
			}
			hrp.Annotations[adoptingAnnotation] = "true"
		} else {
			delete(hrp.Annotations, adoptingAnnotation)
		}
		if err := controllerutil.SetOwnerReference(cluster, hrp, r.Client.Scheme()); err != nil {
			return err
		}

		// CAAPH makes the chart, repository and namespace immutable.
		if hrp.CreationTimestamp.IsZero() {
			hrp.Spec.RepoURL = cat.Chart.RepoURL
			if cni.Spec.ChartRepository != "" {
				hrp.Spec.RepoURL = cni.Spec.ChartRepository
			}
			hrp.Spec.ChartName = cat.Chart.Name
			hrp.Spec.ReleaseNamespace = cat.Release.Namespace
			hrp.Spec.ReconcileStrategy = string(helmv1.ReconcileStrategyContinuous)
		}
		hrp.Spec.ClusterRef = corev1.ObjectReference{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Cluster",
			Namespace:  cluster.Namespace,
			Name:       cluster.Name,
		}
		hrp.Spec.ReleaseName = cat.Release.Name
		hrp.Spec.Version = cat.Format(version)
		hrp.Spec.Values = string(rendered)
		hrp.Spec.Options = cat.HelmOptions
		hrp.Spec.Options.TakeOwnership = true
		hrp.Spec.Options.Install.CreateNamespace = true
		return nil
	})
	return hrp, err
}

// renderInput collects the values template inputs from the Cluster and the
// ClusterCNI.
func renderInput(cluster *clusterv1.Cluster, cni *cniv1.ClusterCNI, adopting bool) (catalog.RenderInput, error) {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
	if err != nil {
		return catalog.RenderInput{}, err
	}
	in := catalog.RenderInput{
		Cluster:              obj,
		Adopting:             adopting,
		KubeProxyReplacement: ptrDeref(cni.Spec.KubeProxyReplacement),
		ServiceCIDRs:         cluster.Spec.ClusterNetwork.Services.CIDRBlocks,
		ImageRepository:      cni.Spec.ImageRepository,
		APIServerHost:        cluster.Spec.ControlPlaneEndpoint.Host,
		APIServerPort:        cluster.Spec.ControlPlaneEndpoint.Port,
	}
	pods := cni.Spec.PodCIDRs
	if len(pods) == 0 {
		pods = cluster.Spec.ClusterNetwork.Pods.CIDRBlocks
	}
	for _, cidr := range pods {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return catalog.RenderInput{}, fmt.Errorf("invalid pod CIDR %q: %w", cidr, err)
		}
		if prefix.Addr().Is4() {
			in.IPv4PodCIDRs = append(in.IPv4PodCIDRs, cidr)
		} else {
			in.IPv6PodCIDRs = append(in.IPv6PodCIDRs, cidr)
		}
	}
	return in, nil
}

func ptrDeref(b *bool) bool {
	return b != nil && *b
}
