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
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

func TestBuiltin(t *testing.T) {
	g := NewWithT(t)

	catalogs, err := Builtin()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(catalogs).To(HaveKey("cilium"))
	g.Expect(catalogs).To(HaveKey("calico"))

	g.Expect(catalogs["cilium"].Format(v("1.16.2"))).To(Equal("1.16.2"))
	g.Expect(catalogs["calico"].Format(v("3.32.1"))).To(Equal("v3.32.1"), "Calico charts carry a v prefix")
	for _, c := range catalogs {
		for _, m := range c.Minors {
			g.Expect(m.CompatibilitySource).ToNot(BeEmpty(), "%s %s needs a compatibilitySource", c.Type, m.Minor)
		}
	}
}

func TestParseRejectsInvalidCatalogs(t *testing.T) {
	valid := strings.TrimSpace(testCatalog)

	tests := map[string]struct {
		data    string
		wantErr string
	}{
		"unknown field":            {data: valid + "\nextra: true", wantErr: "unknown field"},
		"unsorted minors":          {data: strings.Replace(valid, `minor: "1.16", version: 1.16.19`, `minor: "1.14", version: 1.14.19`, 1), wantErr: "sorted"},
		"version of another minor": {data: strings.Replace(valid, "version: 1.17.18", "version: 1.18.1", 1), wantErr: "belongs to another minor"},
		"prerelease version":       {data: strings.Replace(valid, "version: 1.17.18", "version: 1.17.0-rc.1", 1), wantErr: "prerelease"},
		"mixed v prefix":           {data: strings.Replace(valid, "version: 1.17.18", "version: v1.17.18", 1), wantErr: "v prefix"},
		"bad kubernetes range":     {data: strings.Replace(valid, `">= 1.29.0-0, < 1.33.0-0"`, `"not a range"`, 1), wantErr: "kubernetes"},
		"bad template":             {data: strings.Replace(valid, `valuesTemplate: "{}"`, `valuesTemplate: "{{ .Broken"`, 1), wantErr: "values template"},
		"bad detect kind":          {data: strings.Replace(valid, "kind: DaemonSet", "kind: Pod", 1), wantErr: "DaemonSet or Deployment"},
		"missing chart":            {data: strings.Replace(valid, "name: test}", "name: \"\"}", 1), wantErr: "chart"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			_, err := Parse([]byte(tt.data))
			g.Expect(err).To(MatchError(ContainSubstring(tt.wantErr)))
		})
	}
}
