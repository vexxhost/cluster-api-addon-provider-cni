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
	"bytes"
	"fmt"

	"github.com/Masterminds/semver/v3"
	"helm.sh/helm/v3/pkg/chartutil"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// RenderInput is the data a values template sees.
type RenderInput struct {
	// Cluster is the Cluster API Cluster as a generic map, for example
	// .Cluster.spec.clusterNetwork.serviceDomain.
	Cluster map[string]any
	// Version is the CNI version being installed, as the chart names it.
	Version string
	// Adopting is true while an existing install is being taken over.
	Adopting bool
	// KubeProxyReplacement asks the CNI to replace kube-proxy.
	KubeProxyReplacement bool
	// IPv4PodCIDRs and IPv6PodCIDRs are the pod networks.
	IPv4PodCIDRs []string
	IPv6PodCIDRs []string
	// ServiceCIDRs are the service networks of the Cluster.
	ServiceCIDRs []string
	// ImageRepository is a registry mirror for the CNI images, or empty.
	ImageRepository string
	// APIServerHost and APIServerPort are the Cluster's control plane endpoint.
	APIServerHost string
	APIServerPort int32
}

// Values renders the Helm values for version v and merges overlay over them
// using Helm's own rules, so a null in the overlay removes a rendered key.
func (c *Catalog) Values(v *semver.Version, in RenderInput, overlay map[string]any) (map[string]any, error) {
	tmpl := c.tmpl
	if m := c.minorOf(v); m != nil && m.tmpl != nil {
		tmpl = m.tmpl
	}
	in.Version = c.Format(v)

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, in); err != nil {
		return nil, fmt.Errorf("rendering %s values: %w", c.Type, err)
	}
	values := map[string]any{}
	if err := yaml.Unmarshal(buf.Bytes(), &values); err != nil {
		return nil, fmt.Errorf("rendered %s values are not valid YAML: %w", c.Type, err)
	}
	if len(overlay) == 0 {
		return values, nil
	}
	return chartutil.CoalesceTables(runtime.DeepCopyJSON(overlay), values), nil
}
