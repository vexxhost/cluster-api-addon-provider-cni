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

// Package hooks implements the Cluster API lifecycle hooks that keep the CNI
// in step with each control plane step of a (chained) Kubernetes upgrade.
// The hooks only read the ClusterCNI status and the catalog; the controller
// does the work. The one write is the next-version annotation, which tells
// the controller which Kubernetes version comes next.
package hooks

import (
	"context"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/cluster-api/exp/runtime/server"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
)

// retryAfterSeconds is how long CAPI waits before asking again.
const retryAfterSeconds = 15

// Handlers serves the lifecycle hooks.
type Handlers struct {
	Client   client.Client
	Catalogs catalog.Catalogs
}

// Register adds the handlers to a runtime extension server.
func (h *Handlers) Register(s *server.Server) error {
	for _, handler := range []server.ExtensionHandler{
		{Hook: runtimehooksv1.BeforeClusterUpgrade, Name: "before-cluster-upgrade", HandlerFunc: h.BeforeClusterUpgrade},
		{Hook: runtimehooksv1.BeforeControlPlaneUpgrade, Name: "before-control-plane-upgrade", HandlerFunc: h.BeforeControlPlaneUpgrade},
		{Hook: runtimehooksv1.BeforeWorkersUpgrade, Name: "before-workers-upgrade", HandlerFunc: h.BeforeWorkersUpgrade},
	} {
		if err := s.AddExtensionHandler(handler); err != nil {
			return err
		}
	}
	return nil
}

// BeforeClusterUpgrade fails an upgrade whose plan the CNI cannot follow and
// waits while a CNI rollout is in progress.
func (h *Handlers) BeforeClusterUpgrade(ctx context.Context, req *runtimehooksv1.BeforeClusterUpgradeRequest, resp *runtimehooksv1.BeforeClusterUpgradeResponse) {
	cni, cat, observed, ok := h.load(ctx, &req.Cluster, &resp.CommonRetryResponse)
	if !ok {
		return
	}
	if !ready(cni) {
		retry(&resp.CommonRetryResponse, "waiting for the %s rollout to finish", cat.Type)
		return
	}

	steps := req.ControlPlaneUpgrades
	if len(steps) == 0 {
		steps = []runtimehooksv1.UpgradeStepInfo{{Version: req.ToKubernetesVersion}}
	}
	if err := followPlan(cat, cni.Spec.Version, observed, req.FromKubernetesVersion, steps); err != nil {
		fail(&resp.CommonRetryResponse, "%s cannot follow this upgrade: %v", cat.Type, err)
		return
	}
	succeed(&resp.CommonRetryResponse)
}

// BeforeControlPlaneUpgrade announces the step's Kubernetes version to the
// controller and holds the control plane until the CNI supports it.
func (h *Handlers) BeforeControlPlaneUpgrade(ctx context.Context, req *runtimehooksv1.BeforeControlPlaneUpgradeRequest, resp *runtimehooksv1.BeforeControlPlaneUpgradeResponse) {
	cni, cat, observed, ok := h.load(ctx, &req.Cluster, &resp.CommonRetryResponse)
	if !ok {
		return
	}
	to, err := semver.NewVersion(req.ToKubernetesVersion)
	if err != nil {
		fail(&resp.CommonRetryResponse, "invalid Kubernetes version %q: %v", req.ToKubernetesVersion, err)
		return
	}

	if cni.Annotations[cniv1.NextKubernetesVersionAnnotation] != req.ToKubernetesVersion {
		base := cni.DeepCopy()
		if cni.Annotations == nil {
			cni.Annotations = map[string]string{}
		}
		cni.Annotations[cniv1.NextKubernetesVersionAnnotation] = req.ToKubernetesVersion
		if err := h.Client.Patch(ctx, cni, client.MergeFrom(base)); err != nil {
			fail(&resp.CommonRetryResponse, "announcing Kubernetes %s to the CNI controller: %v", req.ToKubernetesVersion, err)
			return
		}
		retry(&resp.CommonRetryResponse, "preparing %s for Kubernetes %s", cat.Type, req.ToKubernetesVersion)
		return
	}

	if !ready(cni) || !cat.Supports(observed, to) {
		retry(&resp.CommonRetryResponse, "waiting for %s to support Kubernetes %s (running %s, target %s)",
			cat.Type, req.ToKubernetesVersion, cni.Status.ObservedVersion, cni.Status.TargetVersion)
		return
	}
	succeed(&resp.CommonRetryResponse)
}

// BeforeWorkersUpgrade holds the workers while the CNI is rolling out.
func (h *Handlers) BeforeWorkersUpgrade(ctx context.Context, req *runtimehooksv1.BeforeWorkersUpgradeRequest, resp *runtimehooksv1.BeforeWorkersUpgradeResponse) {
	cni, cat, _, ok := h.load(ctx, &req.Cluster, &resp.CommonRetryResponse)
	if !ok {
		return
	}
	if !ready(cni) {
		retry(&resp.CommonRetryResponse, "waiting for the %s rollout to %s to finish", cat.Type, cni.Status.TargetVersion)
		return
	}
	succeed(&resp.CommonRetryResponse)
}

// load returns the Cluster's ClusterCNI, its catalog and running version. It
// answers the hook itself and returns false when there is nothing to check
// or the CNI is not installed yet.
func (h *Handlers) load(ctx context.Context, cluster *clusterv1.Cluster, resp *runtimehooksv1.CommonRetryResponse) (*cniv1.ClusterCNI, *catalog.Catalog, *semver.Version, bool) {
	cni := &cniv1.ClusterCNI{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(cluster), cni); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// The Cluster's CNI is not managed by this controller.
			succeed(resp)
		} else {
			fail(resp, "reading the ClusterCNI: %v", err)
		}
		return nil, nil, nil, false
	}
	cat, ok := h.Catalogs[cni.Spec.Type]
	if !ok {
		fail(resp, "no catalog for CNI type %q", cni.Spec.Type)
		return nil, nil, nil, false
	}
	if cni.Status.ObservedVersion == "" {
		retry(resp, "waiting for %s to be installed", cat.Type)
		return nil, nil, nil, false
	}
	observed, err := semver.NewVersion(cni.Status.ObservedVersion)
	if err != nil {
		fail(resp, "invalid observed version %q: %v", cni.Status.ObservedVersion, err)
		return nil, nil, nil, false
	}
	return cni, cat, observed, true
}

// followPlan checks that for every control plane step the CNI can reach a
// version that supports both the Kubernetes version before and after the
// step, along the catalog's upgrade path.
func followPlan(cat *catalog.Catalog, requested string, observed *semver.Version, from string, steps []runtimehooksv1.UpgradeStepInfo) error {
	prev, err := semver.NewVersion(from)
	if err != nil {
		return fmt.Errorf("invalid Kubernetes version %q: %w", from, err)
	}
	current := observed
	for _, step := range steps {
		next, err := semver.NewVersion(step.Version)
		if err != nil {
			return fmt.Errorf("invalid Kubernetes version %q: %w", step.Version, err)
		}
		if !cat.Supports(current, next) {
			target, err := cat.Target(requested, current, prev, next)
			if err != nil {
				return fmt.Errorf("Kubernetes %s to %s: %w", prev, next, err)
			}
			if !cat.Supports(target, prev, next) {
				return fmt.Errorf("%s does not support both Kubernetes %s and %s; %s", cat.Format(target), prev, next, pinHint(requested))
			}
			if _, err := cat.Path(current, target); err != nil {
				return err
			}
			current = target
		}
		prev = next
	}
	return nil
}

func pinHint(requested string) string {
	if requested == "" {
		return "the catalog needs a newer version"
	}
	return fmt.Sprintf("change spec.version (%s) or leave it empty", strings.TrimSpace(requested))
}

// ready reports whether the CNI runs its target version.
func ready(cni *cniv1.ClusterCNI) bool {
	return meta.IsStatusConditionTrue(cni.Status.Conditions, cniv1.ReadyCondition) &&
		cni.Status.ObservedGeneration == cni.Generation &&
		cni.Status.ObservedVersion == cni.Status.TargetVersion
}

func succeed(resp *runtimehooksv1.CommonRetryResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess
	resp.RetryAfterSeconds = 0
}

func retry(resp *runtimehooksv1.CommonRetryResponse, format string, args ...any) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess
	resp.RetryAfterSeconds = retryAfterSeconds
	resp.Message = fmt.Sprintf(format, args...)
}

func fail(resp *runtimehooksv1.CommonRetryResponse, format string, args ...any) {
	resp.Status = runtimehooksv1.ResponseStatusFailure
	resp.Message = fmt.Sprintf(format, args...)
}
