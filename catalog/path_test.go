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
	"testing"

	"github.com/Masterminds/semver/v3"
	. "github.com/onsi/gomega"
)

// testCatalog mirrors the shape of a real catalog but stays fixed, so these
// tests don't change when the built-in catalog is updated.
const testCatalog = `
type: test
chart: {repoURL: https://charts.example.com, name: test}
release: {name: test, namespace: kube-system}
detect: [{kind: DaemonSet, namespace: kube-system, name: test, container: agent}]
valuesTemplate: "{}"
minors:
  - {minor: "1.15", version: 1.15.19, kubernetes: ">= 1.26.0-0, < 1.30.0-0"}
  - {minor: "1.16", version: 1.16.19, kubernetes: ">= 1.27.0-0, < 1.31.0-0"}
  - {minor: "1.17", version: 1.17.18, kubernetes: ">= 1.29.0-0, < 1.33.0-0"}
  - {minor: "1.18", version: 1.18.17, kubernetes: ">= 1.30.0-0, < 1.34.0-0"}
  - {minor: "1.19", version: 1.19.11, kubernetes: ">= 1.32.0-0, < 1.36.0-0"}
`

func mustParse(t *testing.T, data string) *Catalog {
	t.Helper()
	c, err := Parse([]byte(data))
	NewWithT(t).Expect(err).ToNot(HaveOccurred())
	return c
}

func v(s string) *semver.Version {
	if s == "" {
		return nil
	}
	return semver.MustParse(s)
}

func versions(vs ...string) []*semver.Version {
	out := make([]*semver.Version, 0, len(vs))
	for _, s := range vs {
		out = append(out, v(s))
	}
	return out
}

func TestTarget(t *testing.T) {
	c := mustParse(t, testCatalog)

	tests := []struct {
		name       string
		requested  string
		observed   string
		kubernetes []string
		want       string
		wantErr    error
	}{
		{name: "auto picks the newest supporting minor", kubernetes: []string{"v1.32.4"}, want: "1.19.11"},
		{name: "auto must support the next version too", kubernetes: []string{"v1.29.0", "v1.30.2"}, want: "1.17.18"},
		{name: "auto keeps a newer running patch", observed: "1.19.20", kubernetes: []string{"v1.32.0"}, want: "1.19.20"},
		{name: "auto never downgrades", observed: "1.19.5", kubernetes: []string{"v1.31.0"}, want: "1.19.5"},
		{name: "auto with no supporting minor", kubernetes: []string{"v1.40.0"}, wantErr: ErrNoCompatibleVersion},
		{name: "minor follows its latest patch", requested: "1.17", observed: "1.15.3", want: "1.17.18"},
		{name: "minor accepts a v prefix", requested: "v1.17", want: "1.17.18"},
		{name: "minor keeps a newer running patch", requested: "1.17", observed: "1.17.20", want: "1.17.20"},
		{name: "exact pins a patch", requested: "1.16.5", observed: "1.15.3", want: "1.16.5"},
		{name: "exact ignores Kubernetes ranges", requested: "1.15.3", kubernetes: []string{"v1.33.0"}, want: "1.15.3"},
		{name: "exact downgrade", requested: "1.15.3", observed: "1.16.0", wantErr: ErrDowngrade},
		{name: "minor downgrade", requested: "1.16", observed: "1.17.1", wantErr: ErrDowngrade},
		{name: "unknown minor", requested: "1.12", wantErr: ErrUnknownVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			got, err := c.Target(tt.requested, v(tt.observed), versions(tt.kubernetes...)...)
			if tt.wantErr != nil {
				g.Expect(err).To(MatchError(tt.wantErr))
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got.String()).To(Equal(tt.want))
		})
	}
}

func TestPath(t *testing.T) {
	c := mustParse(t, testCatalog)

	tests := []struct {
		name     string
		catalog  string
		observed string
		target   string
		want     []string
		wantErr  error
	}{
		{name: "fresh install goes straight to the target", target: "1.19.11", want: []string{"1.19.11"}},
		{name: "same minor goes straight to the target", observed: "1.16.3", target: "1.16.19", want: []string{"1.16.19"}},
		{
			name:     "minors step one at a time from the latest patch",
			observed: "1.15.3", target: "1.19.11",
			want: []string{"1.15.19", "1.16.19", "1.17.18", "1.18.17", "1.19.11"},
		},
		{name: "a pinned patch ends the path", observed: "1.16.19", target: "1.17.2", want: []string{"1.17.2"}},
		{name: "already there", observed: "1.18.17", target: "1.18.17", want: nil},
		{name: "unknown running minor", observed: "1.10.2", target: "1.16.19", wantErr: ErrUnknownVersion},
		{
			name: "a missing minor breaks the path",
			catalog: `
type: test
chart: {repoURL: https://charts.example.com, name: test}
release: {name: test, namespace: kube-system}
detect: [{kind: DaemonSet, namespace: kube-system, name: test, container: agent}]
valuesTemplate: "{}"
minors:
  - {minor: "1.15", version: 1.15.19, kubernetes: ">= 1.26.0-0"}
  - {minor: "1.17", version: 1.17.18, kubernetes: ">= 1.29.0-0"}
`,
			observed: "1.15.19", target: "1.17.18", wantErr: ErrNoUpgradePath,
		},
		{
			name: "upgradeFrom replaces the minor skip rule",
			catalog: `
type: test
chart: {repoURL: https://charts.example.com, name: test}
release: {name: test, namespace: kube-system}
detect: [{kind: DaemonSet, namespace: kube-system, name: test, container: agent}]
valuesTemplate: "{}"
minors:
  - {minor: "1.15", version: 1.15.19, kubernetes: ">= 1.26.0-0"}
  - {minor: "1.17", version: 1.17.18, kubernetes: ">= 1.29.0-0", upgradeFrom: ">= 1.15.0"}
`,
			observed: "1.15.19", target: "1.17.18", want: []string{"1.17.18"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			cat := c
			if tt.catalog != "" {
				cat = mustParse(t, tt.catalog)
			}
			got, err := cat.Path(v(tt.observed), v(tt.target))
			if tt.wantErr != nil {
				g.Expect(err).To(MatchError(tt.wantErr))
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			var gotStrings []string
			for _, s := range got {
				gotStrings = append(gotStrings, s.String())
			}
			g.Expect(gotStrings).To(Equal(tt.want))
		})
	}
}

func TestSupports(t *testing.T) {
	g := NewWithT(t)
	c := mustParse(t, testCatalog)

	g.Expect(c.Supports(v("1.18.2"), v("v1.33.2+k3s1"))).To(BeTrue(), "build metadata is ignored")
	g.Expect(c.Supports(v("1.18.2"), v("v1.33.0-rc.1"))).To(BeTrue(), "prereleases count as their release")
	g.Expect(c.Supports(v("1.18.2"), v("v1.30.0"), v("v1.34.0"))).To(BeFalse(), "every version must be supported")
	g.Expect(c.Supports(v("1.18.2"), v("v1.30.0"), nil)).To(BeTrue(), "nil versions are skipped")
	g.Expect(c.Supports(v("1.12.0"), v("v1.30.0"))).To(BeFalse(), "unknown minors support nothing")
}
