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

// Package controllers holds the ClusterCNI reconciler.
package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	helmv1 "sigs.k8s.io/cluster-api-addon-provider-helm/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"

	cniv1 "github.com/vexxhost/cluster-api-addon-provider-cni/api/v1alpha1"
	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
)

// ControllerName is the name of the ClusterCNI controller.
const ControllerName = "clustercni"

// connectRetry is how long to wait for a workload cluster connection.
const connectRetry = 15 * time.Second

// ClusterCNIReconciler installs, upgrades and takes over the CNI of the
// Cluster with the same name as each ClusterCNI.
type ClusterCNIReconciler struct {
	Client client.Client
	// APIReader reads objects the manager doesn't cache: control plane objects
	// and ClusterResourceSet Secrets.
	APIReader        client.Reader
	ClusterCache     clustercache.ClusterCache
	Catalogs         catalog.Catalogs
	WatchFilterValue string

	// crdCache holds rendered CRD charts.
	crdCache sync.Map
}

// Changing ownerReferences needs delete on the object and, for
// blockOwnerDeletion, update on the owner's finalizers.
// +kubebuilder:rbac:groups=cni.vexxhost.com,resources=clustercnis,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=cni.vexxhost.com,resources=clustercnis/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=*,verbs=get
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmreleaseproxies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=clusterresourcesets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterCNIReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	log := ctrl.LoggerFrom(ctx)
	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&cniv1.ClusterCNI{}).
		// A ClusterCNI has the name of its Cluster.
		Watches(&clusterv1.Cluster{}, &handler.EnqueueRequestForObject{}).
		Watches(&helmv1.HelmReleaseProxy{}, handler.EnqueueRequestsFromMapFunc(helmReleaseProxyToClusterCNI)).
		// Reconcile as soon as the workload cluster connection comes up.
		WatchesRawSource(r.ClusterCache.GetClusterSource(ControllerName, clusterToClusterCNI)).
		WithOptions(options).
		WithEventFilter(predicates.ResourceHasFilterLabel(mgr.GetScheme(), log, r.WatchFilterValue)).
		Complete(r)
}

func clusterToClusterCNI(_ context.Context, o client.Object) []ctrl.Request {
	return []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(o)}}
}

func helmReleaseProxyToClusterCNI(_ context.Context, o client.Object) []ctrl.Request {
	name, ok := o.GetLabels()[cniv1.ClusterCNINameLabel]
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: o.GetNamespace(), Name: name}}}
}

// Reconcile moves the CNI of one Cluster one step towards its target version.
func (r *ClusterCNIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	cni := &cniv1.ClusterCNI{}
	if err := r.Client.Get(ctx, req.NamespacedName, cni); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cni.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		// The Cluster watch reconciles again once it exists.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if annotations.IsPaused(cluster, cni) {
		return ctrl.Result{}, nil
	}

	helper, err := patch.NewHelper(cni, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		if err := helper.Patch(ctx, cni,
			patch.WithOwnedConditions{Conditions: []string{cniv1.ReadyCondition, cniv1.UpgradeCompatibleCondition}},
			patch.WithStatusObservedGeneration{},
		); err != nil {
			reterr = errors.Join(reterr, err)
		}
	}()

	if err := controllerutil.SetOwnerReference(cluster, cni, r.Client.Scheme()); err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcile(ctx, cluster, cni)
}

func (r *ClusterCNIReconciler) reconcile(ctx context.Context, cluster *clusterv1.Cluster, cni *cniv1.ClusterCNI) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	cat, ok := r.Catalogs[cni.Spec.Type]
	if !ok {
		setReady(cni, metav1.ConditionFalse, cniv1.UnknownVersionReason, "no catalog for CNI type %q", cni.Spec.Type)
		return ctrl.Result{}, nil
	}

	if !ptr.Deref(cluster.Status.Initialization.ControlPlaneInitialized, false) {
		setReady(cni, metav1.ConditionFalse, cniv1.WaitingForControlPlaneReason, "waiting for the control plane to be initialized")
		setCompatible(cni, metav1.ConditionUnknown, cniv1.UnknownReason, "the control plane is not initialized")
		return ctrl.Result{}, nil
	}

	// Kubernetes versions: what runs now, what the cluster moves to, and the
	// next step announced through the annotation.
	key := client.ObjectKeyFromObject(cluster)
	running, err := r.runningKubernetesVersion(ctx, key)
	if errors.Is(err, clustercache.ErrClusterNotConnected) {
		setReady(cni, metav1.ConditionFalse, cniv1.WaitingForControlPlaneReason, "waiting for a connection to the workload cluster")
		return ctrl.Result{RequeueAfter: connectRetry}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	desired := r.desiredKubernetesVersion(ctx, cluster)
	announced := announcedKubernetesVersion(ctx, cni)
	if announced != nil && !release(running).LessThan(release(announced)) {
		delete(cni.Annotations, cniv1.NextKubernetesVersionAnnotation)
		announced = nil
	}
	next := announced
	if next == nil && desired != nil && release(running).LessThan(release(desired)) {
		next = desired
	}

	// The running CNI version.
	hrp, err := r.getHelmReleaseProxy(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	var observed *semver.Version
	adopting, inProgress := false, false
	switch {
	case hrp == nil:
		observed, err = r.detect(ctx, key, cat)
		if errors.Is(err, clustercache.ErrClusterNotConnected) {
			return ctrl.Result{RequeueAfter: connectRetry}, nil
		}
		if err != nil {
			setReady(cni, metav1.ConditionFalse, cniv1.UnknownVersionReason, "%v", err)
			return ctrl.Result{}, nil
		}
		if observed != nil && !cat.Known(observed) {
			setReady(cni, metav1.ConditionFalse, cniv1.UnknownVersionReason,
				"found %s %s installed, but its minor version is not in the catalog", cat.Type, cat.Format(observed))
			return ctrl.Result{}, nil
		}
		adopting = observed != nil
	case helmReleaseProxyReady(hrp):
		observed, err = semver.NewVersion(hrp.Spec.Version)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("HelmReleaseProxy %s has an invalid version: %w", hrp.Name, err)
		}
	default:
		inProgress = true
		adopting = hrp.Annotations[adoptingAnnotation] == "true"
		if cni.Status.ObservedVersion != "" {
			observed, err = semver.NewVersion(cni.Status.ObservedVersion)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if observed != nil {
		cni.Status.ObservedVersion = cat.Format(observed)
		setCompatibility(cni, cat, observed, running, desired, announced)
	} else {
		setCompatible(cni, metav1.ConditionUnknown, cniv1.UnknownReason, "the CNI is not installed yet")
	}

	// The target version and the next step towards it.
	target, err := cat.Target(cni.Spec.Version, observed, versions(running, next)...)
	if errors.Is(err, catalog.ErrNoCompatibleVersion) && next != nil {
		target, err = cat.Target(cni.Spec.Version, observed, running)
	}
	switch {
	case errors.Is(err, catalog.ErrNoCompatibleVersion) && observed != nil:
		// Keep the running version until the catalog supports the cluster.
		target = observed
	case errors.Is(err, catalog.ErrNoCompatibleVersion):
		setReady(cni, metav1.ConditionFalse, cniv1.NoCompatibleVersionReason, "no %s version in the catalog supports Kubernetes %s", cat.Type, running)
		return ctrl.Result{}, nil
	case errors.Is(err, catalog.ErrDowngrade):
		setReady(cni, metav1.ConditionFalse, cniv1.DowngradeRejectedReason, "%v", err)
		return ctrl.Result{}, nil
	case err != nil:
		setReady(cni, metav1.ConditionFalse, cniv1.UnknownVersionReason, "%v", err)
		return ctrl.Result{}, nil
	}
	cni.Status.TargetVersion = cat.Format(target)

	var hop *semver.Version
	switch {
	case inProgress:
		// Finish the step that is rolling out before taking the next one.
		if hop, err = semver.NewVersion(hrp.Spec.Version); err != nil {
			return ctrl.Result{}, err
		}
	case adopting:
		hop = observed
	default:
		hop, err = cat.NextHop(observed, target)
		if err != nil {
			setReady(cni, metav1.ConditionFalse, cniv1.NoUpgradePathReason, "%v", err)
			return ctrl.Result{}, nil
		}
	}

	if hrp == nil || hrp.Spec.Version != cat.Format(hop) {
		name, err := r.conflictingClusterResourceSet(ctx, cluster, cat)
		if err != nil {
			return ctrl.Result{}, err
		}
		if name != "" {
			setReady(cni, metav1.ConditionFalse, cniv1.ConflictingClusterResourceSetReason,
				"ClusterResourceSet %s uses the Reconcile strategy and applies %s; change it to ApplyOnce or remove the CNI from it", name, cat.Type)
			return ctrl.Result{}, nil
		}
	}

	if err := r.ensureNamespace(ctx, key, cat.Release); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating the %s namespace: %w", cat.Release.Namespace, err)
	}

	// Helm can't be trusted with the CRDs: it never upgrades a chart's crds/
	// directory, and some charts move their CRDs to a separate chart.
	if cni.Status.CRDVersion != cat.Format(hop) {
		if err := r.applyCRDs(ctx, key, cni, cat, hop, running); err != nil {
			setProgress(cni, cat, adopting, observed, target, hop, "the CRDs for ", nil)
			return ctrl.Result{}, fmt.Errorf("applying the CRDs for %s %s: %w", cat.Type, cat.Format(hop), err)
		}
		cni.Status.CRDVersion = cat.Format(hop)
	}

	hrp, err = r.applyHelmReleaseProxy(ctx, cluster, cni, cat, hop, adopting)
	if err != nil {
		return ctrl.Result{}, err
	}

	if helmReleaseProxyReady(hrp) && observed != nil && observed.Equal(target) && hrp.Spec.Version == cat.Format(target) {
		setReady(cni, metav1.ConditionTrue, cniv1.ReadyReason, "%s %s is running", cat.Type, cat.Format(target))
	} else {
		setProgress(cni, cat, adopting, observed, target, hop, "", hrp)
	}
	log.V(4).Info("Reconciled CNI", "observed", cni.Status.ObservedVersion, "target", cni.Status.TargetVersion, "step", cat.Format(hop))
	return ctrl.Result{}, nil
}

// runningKubernetesVersion returns the workload cluster's API server version.
func (r *ClusterCNIReconciler) runningKubernetesVersion(ctx context.Context, key client.ObjectKey) (*semver.Version, error) {
	cfg, err := r.ClusterCache.GetRESTConfig(ctx, key)
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	info, err := dc.ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("reading the workload cluster version: %w", err)
	}
	return semver.NewVersion(info.GitVersion)
}

// announcedKubernetesVersion parses the next-version annotation, ignoring an
// invalid value.
func announcedKubernetesVersion(ctx context.Context, cni *cniv1.ClusterCNI) *semver.Version {
	value, ok := cni.Annotations[cniv1.NextKubernetesVersionAnnotation]
	if !ok {
		return nil
	}
	v, err := semver.NewVersion(value)
	if err != nil {
		ctrl.LoggerFrom(ctx).Info("Ignoring invalid annotation", "annotation", cniv1.NextKubernetesVersionAnnotation, "value", value)
		return nil
	}
	return v
}

func setCompatibility(cni *cniv1.ClusterCNI, cat *catalog.Catalog, observed *semver.Version, kubernetes ...*semver.Version) {
	for _, k := range kubernetes {
		if k != nil && !cat.Supports(observed, k) {
			setCompatible(cni, metav1.ConditionFalse, cniv1.IncompatibleReason, "%s %s does not support Kubernetes %s", cat.Type, cat.Format(observed), k)
			return
		}
	}
	setCompatible(cni, metav1.ConditionTrue, cniv1.CompatibleReason, "%s %s supports the cluster's Kubernetes versions", cat.Type, cat.Format(observed))
}

// setProgress reports what the controller is waiting for on the way to target.
func setProgress(cni *cniv1.ClusterCNI, cat *catalog.Catalog, adopting bool, observed, target, hop *semver.Version, what string, hrp *helmv1.HelmReleaseProxy) {
	switch {
	case adopting:
		setReady(cni, metav1.ConditionFalse, cniv1.AdoptingReason, "taking over %s%s %s%s", what, cat.Type, cat.Format(hop), releaseMessage(hrp))
	case observed == nil:
		setReady(cni, metav1.ConditionFalse, cniv1.InstallingReason, "installing %s%s %s%s", what, cat.Type, cat.Format(hop), releaseMessage(hrp))
	default:
		setReady(cni, metav1.ConditionFalse, cniv1.UpgradingReason, "upgrading %s%s from %s to %s, now at step %s%s",
			what, cat.Type, cat.Format(observed), cat.Format(target), cat.Format(hop), releaseMessage(hrp))
	}
}

func setReady(cni *cniv1.ClusterCNI, status metav1.ConditionStatus, reason, format string, args ...any) {
	conditions.Set(cni, metav1.Condition{Type: cniv1.ReadyCondition, Status: status, Reason: reason, Message: fmt.Sprintf(format, args...)})
}

func setCompatible(cni *cniv1.ClusterCNI, status metav1.ConditionStatus, reason, format string, args ...any) {
	conditions.Set(cni, metav1.Condition{Type: cniv1.UpgradeCompatibleCondition, Status: status, Reason: reason, Message: fmt.Sprintf(format, args...)})
}

func versions(vs ...*semver.Version) []*semver.Version {
	out := make([]*semver.Version, 0, len(vs))
	for _, v := range vs {
		if v != nil {
			out = append(out, v)
		}
	}
	return out
}

// release drops prerelease and build metadata from a Kubernetes version.
func release(v *semver.Version) *semver.Version {
	return semver.New(v.Major(), v.Minor(), v.Patch(), "", "")
}

// ignoreNotFound returns nil for a NotFound error.
func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
