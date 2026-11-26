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

// Package catalog holds the built-in CNI catalog: for every supported CNI,
// the Helm chart to install, how to find an existing install, the values to
// render, and for every minor version its latest patch, the Kubernetes
// versions it supports and the versions it can be upgraded from.
package catalog

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"text/template"

	"github.com/Masterminds/semver/v3"
	"github.com/Masterminds/sprig/v3"
	helmv1 "sigs.k8s.io/cluster-api-addon-provider-helm/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

//go:embed *.yaml
var builtin embed.FS

// Catalog describes how to manage one CNI.
type Catalog struct {
	// Type is the CNI type, matching ClusterCNI spec.type.
	Type string `json:"type"`
	// Chart is the Helm chart that installs the CNI.
	Chart Chart `json:"chart"`
	// Release is the Helm release name and namespace in the workload cluster.
	Release Release `json:"release"`
	// Detect lists workloads whose container image tag gives the version of
	// a CNI that was installed without this controller. The first match wins.
	Detect []DetectTarget `json:"detect"`
	// CRDChart is an optional chart with CRDs that must be installed before
	// the main chart, at the same version.
	CRDChart *CRDChart `json:"crdChart,omitempty"`
	// HelmOptions are passed to the HelmReleaseProxy.
	HelmOptions helmv1.HelmOptions `json:"helmOptions,omitempty"`
	// MaxMinorSkip is how many minor versions one upgrade step may move.
	// Defaults to 1.
	MaxMinorSkip uint64 `json:"maxMinorSkip,omitempty"`
	// ValuesTemplate renders the Helm values. See RenderInput for its inputs.
	ValuesTemplate string `json:"valuesTemplate"`
	// Minors lists the supported minor versions, oldest first.
	Minors []Minor `json:"minors"`

	// prefix is "v" when the chart versions carry it, like Calico's.
	prefix string
	tmpl   *template.Template
}

// Chart is a Helm chart in a repository.
type Chart struct {
	// RepoURL is the chart repository, an https:// or oci:// URL.
	RepoURL string `json:"repoURL"`
	// Name is the chart name.
	Name string `json:"name"`
}

// CRDChart is a chart in the same repository that installs CRDs the main
// chart needs before its own resources can be created.
type CRDChart struct {
	// Name is the chart name.
	Name string `json:"name"`
	// Release is the Helm release name, in the namespace of the main release.
	Release string `json:"release"`
	// Versions is the semver constraint of the CNI versions that need it.
	Versions string `json:"versions"`

	versions *semver.Constraints
}

// Release is a Helm release in the workload cluster.
type Release struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// DetectTarget is a workload container whose image tag is the CNI version.
type DetectTarget struct {
	// Kind is DaemonSet or Deployment.
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Container string `json:"container"`
}

// Minor is one minor version of a CNI.
type Minor struct {
	// Minor is the minor version, e.g. "1.16".
	Minor string `json:"minor"`
	// Version is the latest patch of this minor, as the chart names it.
	Version string `json:"version"`
	// Kubernetes is the semver constraint of the supported Kubernetes versions.
	Kubernetes string `json:"kubernetes"`
	// CompatibilitySource links the upstream page the Kubernetes range comes from.
	CompatibilitySource string `json:"compatibilitySource,omitempty"`
	// UpgradeFrom optionally replaces MaxMinorSkip with a semver constraint of
	// the versions this minor can be upgraded from.
	UpgradeFrom string `json:"upgradeFrom,omitempty"`
	// ValuesTemplate optionally replaces the catalog's values template.
	ValuesTemplate string `json:"valuesTemplate,omitempty"`

	minor       *semver.Version
	version     *semver.Version
	kubernetes  *semver.Constraints
	upgradeFrom *semver.Constraints
	tmpl        *template.Template
}

// Catalogs maps CNI types to their catalogs.
type Catalogs map[string]*Catalog

// Builtin returns the catalogs compiled into the binary.
func Builtin() (Catalogs, error) {
	return Load(builtin)
}

// Load parses and validates every *.yaml file at the root of fsys.
func Load(fsys fs.FS) (Catalogs, error) {
	files, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return nil, err
	}
	catalogs := Catalogs{}
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		c, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path.Base(f), err)
		}
		if _, dup := catalogs[c.Type]; dup {
			return nil, fmt.Errorf("%s: duplicate catalog type %q", path.Base(f), c.Type)
		}
		catalogs[c.Type] = c
	}
	return catalogs, nil
}

// Parse parses and validates one catalog file. Unknown fields are errors.
func Parse(data []byte) (*Catalog, error) {
	c := &Catalog{}
	if err := yaml.UnmarshalStrict(data, c); err != nil {
		return nil, err
	}
	if err := c.complete(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Catalog) complete() error {
	switch {
	case c.Type == "":
		return fmt.Errorf("type is required")
	case c.Chart.RepoURL == "" || c.Chart.Name == "":
		return fmt.Errorf("chart.repoURL and chart.name are required")
	case c.Release.Name == "" || c.Release.Namespace == "":
		return fmt.Errorf("release.name and release.namespace are required")
	case len(c.Detect) == 0:
		return fmt.Errorf("detect needs at least one target")
	case len(c.Minors) == 0:
		return fmt.Errorf("minors needs at least one entry")
	}
	for _, d := range c.Detect {
		if d.Kind != "DaemonSet" && d.Kind != "Deployment" {
			return fmt.Errorf("detect kind %q must be DaemonSet or Deployment", d.Kind)
		}
		if d.Namespace == "" || d.Name == "" || d.Container == "" {
			return fmt.Errorf("detect targets need namespace, name and container")
		}
	}
	if c.MaxMinorSkip == 0 {
		c.MaxMinorSkip = 1
	}
	if c.CRDChart != nil {
		if c.CRDChart.Name == "" || c.CRDChart.Release == "" {
			return fmt.Errorf("crdChart.name and crdChart.release are required")
		}
		v, err := semver.NewConstraint(c.CRDChart.Versions)
		if err != nil {
			return fmt.Errorf("crdChart.versions: %w", err)
		}
		c.CRDChart.versions = v
	}

	var err error
	if c.tmpl, err = newTemplate(c.Type, c.ValuesTemplate); err != nil {
		return err
	}

	c.prefix = ""
	if strings.HasPrefix(c.Minors[0].Version, "v") {
		c.prefix = "v"
	}
	for i := range c.Minors {
		m := &c.Minors[i]
		if m.minor, err = semver.StrictNewVersion(m.Minor + ".0"); err != nil {
			return fmt.Errorf("minor %q: must be MAJOR.MINOR", m.Minor)
		}
		if m.version, err = semver.NewVersion(m.Version); err != nil {
			return fmt.Errorf("minor %s: version: %w", m.Minor, err)
		}
		if m.version.Prerelease() != "" {
			return fmt.Errorf("minor %s: version %s is a prerelease", m.Minor, m.Version)
		}
		if m.version.Major() != m.minor.Major() || m.version.Minor() != m.minor.Minor() {
			return fmt.Errorf("minor %s: version %s belongs to another minor", m.Minor, m.Version)
		}
		if strings.HasPrefix(m.Version, "v") != (c.prefix == "v") {
			return fmt.Errorf("minor %s: version %s must use the same v prefix as the other versions", m.Minor, m.Version)
		}
		if m.kubernetes, err = semver.NewConstraint(m.Kubernetes); err != nil {
			return fmt.Errorf("minor %s: kubernetes: %w", m.Minor, err)
		}
		if m.UpgradeFrom != "" {
			if m.upgradeFrom, err = semver.NewConstraint(m.UpgradeFrom); err != nil {
				return fmt.Errorf("minor %s: upgradeFrom: %w", m.Minor, err)
			}
		}
		if m.ValuesTemplate != "" {
			if m.tmpl, err = newTemplate(c.Type+"-"+m.Minor, m.ValuesTemplate); err != nil {
				return err
			}
		}
		if i > 0 && !c.Minors[i-1].minor.LessThan(m.minor) {
			return fmt.Errorf("minors must be sorted oldest first and unique, %s follows %s", m.Minor, c.Minors[i-1].Minor)
		}
	}
	return nil
}

// CRDChartFor returns the CRD chart version v needs, or nil.
func (c *Catalog) CRDChartFor(v *semver.Version) *CRDChart {
	if c.CRDChart == nil || !c.CRDChart.versions.Check(release(v)) {
		return nil
	}
	return c.CRDChart
}

// Format returns v the way the chart names its versions.
func (c *Catalog) Format(v *semver.Version) string {
	return c.prefix + v.String()
}

// minorOf returns the catalog entry for v's minor version.
func (c *Catalog) minorOf(v *semver.Version) *Minor {
	for i := range c.Minors {
		m := &c.Minors[i]
		if m.minor.Major() == v.Major() && m.minor.Minor() == v.Minor() {
			return m
		}
	}
	return nil
}

func newTemplate(name, text string) (*template.Template, error) {
	t, err := template.New(name).Option("missingkey=zero").Funcs(sprig.TxtFuncMap()).Funcs(template.FuncMap{
		"toYaml": func(v any) (string, error) {
			out, err := yaml.Marshal(v)
			return strings.TrimSuffix(string(out), "\n"), err
		},
	}).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("values template %s: %w", name, err)
	}
	return t, nil
}
