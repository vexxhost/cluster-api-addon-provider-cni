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

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// NextKubernetesVersionAnnotation tells the controller which Kubernetes
	// version the cluster moves to next. The controller then stages a CNI
	// version that supports both the running and the next Kubernetes version.
	// The runtime hooks set it before each control plane upgrade step; any other
	// orchestrator can set it before bumping the Kubernetes version. The
	// controller removes it once the cluster runs that version.
	NextKubernetesVersionAnnotation = "cni.vexxhost.com/next-kubernetes-version"

	// ClusterCNINameLabel is set on the HelmReleaseProxy managed for a ClusterCNI.
	ClusterCNINameLabel = "cni.vexxhost.com/clustercni-name"
)

// Condition types.
const (
	// ReadyCondition is True when the CNI runs the target version and its Helm
	// release is deployed.
	ReadyCondition = "Ready"

	// UpgradeCompatibleCondition is True when the running CNI version supports
	// the cluster's target Kubernetes version and any pending next version.
	UpgradeCompatibleCondition = "UpgradeCompatible"
)

// Condition reasons.
const (
	// ReadyReason means the CNI runs the target version.
	ReadyReason = "Ready"
	// InstallingReason means the CNI is being installed on a cluster without one.
	InstallingReason = "Installing"
	// AdoptingReason means an existing CNI is being taken over at its running version.
	AdoptingReason = "Adopting"
	// UpgradingReason means the CNI is moving to the next version on its upgrade path.
	UpgradingReason = "Upgrading"
	// WaitingForControlPlaneReason means the workload cluster API is not available yet.
	WaitingForControlPlaneReason = "WaitingForControlPlane"
	// NoUpgradePathReason means the catalog has no allowed path to the target version.
	NoUpgradePathReason = "NoUpgradePath"
	// NoCompatibleVersionReason means no catalog version supports the cluster's Kubernetes version.
	NoCompatibleVersionReason = "NoCompatibleVersion"
	// DowngradeRejectedReason means the requested version is older than the running one.
	DowngradeRejectedReason = "DowngradeRejected"
	// UnknownVersionReason means the requested or running version is not in the catalog.
	UnknownVersionReason = "UnknownVersion"
	// ConflictingClusterResourceSetReason means a ClusterResourceSet with the
	// Reconcile strategy keeps applying this CNI and would fight the controller.
	ConflictingClusterResourceSetReason = "ConflictingClusterResourceSet"
	// CompatibleReason means the running CNI supports the target Kubernetes versions.
	CompatibleReason = "Compatible"
	// IncompatibleReason means the running CNI does not support a target Kubernetes version.
	IncompatibleReason = "Incompatible"
	// UnknownReason means compatibility cannot be determined yet.
	UnknownReason = "Unknown"
)

// ClusterCNISpec defines the desired CNI of a Cluster.
// +kubebuilder:validation:XValidation:rule="has(self.chartRepository) == has(oldSelf.chartRepository) && (!has(self.chartRepository) || self.chartRepository == oldSelf.chartRepository)",message="chartRepository is immutable"
type ClusterCNISpec struct {
	// type is the CNI to install.
	// +required
	// +kubebuilder:validation:Enum=cilium;calico
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable"
	Type string `json:"type"`

	// version selects the CNI version. Leave it empty to follow the newest
	// version that supports the cluster's Kubernetes version. A minor version
	// such as "1.16" follows that minor's latest patch, and a full version such
	// as "1.16.5" pins that patch.
	// +optional
	// +kubebuilder:validation:Pattern=`^v?[0-9]+\.[0-9]+(\.[0-9]+)?$`
	Version string `json:"version,omitempty"`

	// kubeProxyReplacement makes the CNI replace kube-proxy, where supported.
	// +optional
	KubeProxyReplacement *bool `json:"kubeProxyReplacement,omitempty"`

	// podCIDRs are the pod networks. Defaults to the Cluster's
	// spec.clusterNetwork.pods.cidrBlocks.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=2
	PodCIDRs []string `json:"podCIDRs,omitempty"`

	// imageRepository is a registry mirror for the CNI images.
	// +optional
	ImageRepository string `json:"imageRepository,omitempty"`

	// chartRepository replaces the Helm chart repository from the catalog,
	// for example with an OCI mirror. It is immutable.
	// +optional
	ChartRepository string `json:"chartRepository,omitempty"`

	// providerConfig holds Helm values merged over the values the controller
	// renders, for settings that the fields above do not cover.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	ProviderConfig *apiextensionsv1.JSON `json:"providerConfig,omitempty"`
}

// ClusterCNIStatus defines the observed state of a ClusterCNI.
type ClusterCNIStatus struct {
	// observedVersion is the CNI version running in the cluster.
	// +optional
	ObservedVersion string `json:"observedVersion,omitempty"`

	// targetVersion is the CNI version the controller is moving to.
	// +optional
	TargetVersion string `json:"targetVersion,omitempty"`

	// observedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the observations of the ClusterCNI's current state.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterCNI manages the CNI of the Cluster API Cluster with the same name.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=clustercnis,shortName=ccni,categories=cluster-api
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.observedVersion"
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".status.targetVersion"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type ClusterCNI struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired CNI.
	// +required
	Spec ClusterCNISpec `json:"spec"`

	// status is the observed state.
	// +optional
	Status ClusterCNIStatus `json:"status,omitempty"`
}

// GetConditions returns the conditions of the ClusterCNI.
func (c *ClusterCNI) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// SetConditions sets the conditions of the ClusterCNI.
func (c *ClusterCNI) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}

// ClusterCNIList contains a list of ClusterCNI.
// +kubebuilder:object:root=true
type ClusterCNIList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterCNI `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterCNI{}, &ClusterCNIList{})
}
