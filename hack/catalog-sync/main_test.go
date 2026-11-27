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

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
)

func fixtureIndex(versions ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nentries:\n  test:\n")
	for _, v := range versions {
		fmt.Fprintf(&b, "  - name: test\n    version: %s\n    urls: [test-%s.tgz]\n", v, v)
	}
	return b.String()
}

const fixtureCatalog = `# A test catalog; this comment must survive.
type: test
chart:
  repoURL: %s
  name: test
release:
  name: test
  namespace: kube-system
detect:
  - kind: DaemonSet
    namespace: kube-system
    name: test
    container: agent
valuesTemplate: |
  {{- with .ImageRepository }}
  image: {{ . }}/agent
  {{- end }}
minors:
  - minor: "1.15"
    version: 1.15.19
    kubernetes: ">= 1.26.0-0, < 1.30.0-0"
    compatibilitySource: https://docs.example.com/v1.15/compatibility/
  - minor: "1.16"
    version: 1.16.19
    kubernetes: ">= 1.27.0-0, < 1.31.0-0"
    compatibilitySource: https://docs.example.com/v1.16/compatibility/
`

func setup(t *testing.T, versions ...string) (string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index.yaml" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(fixtureIndex(versions...)))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	NewWithT(t).Expect(os.WriteFile(path, []byte(fmt.Sprintf(fixtureCatalog, srv.URL)), 0o600)).To(Succeed())
	return dir, path
}

func load(t *testing.T, path string) (*catalog.Catalog, string) {
	t.Helper()
	g := NewWithT(t)
	data, err := os.ReadFile(path)
	g.Expect(err).ToNot(HaveOccurred())
	c, err := catalog.Parse(data)
	g.Expect(err).ToNot(HaveOccurred())
	return c, string(data)
}

func TestPatchMode(t *testing.T) {
	g := NewWithT(t)
	dir, path := setup(t, "1.15.19", "1.15.20", "1.16.19", "1.16.21", "1.16.22-rc.1", "1.17.3")

	changes, err := syncDir(dir, "patch")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(changes).To(ConsistOf(
		"- test 1.15: 1.15.19 → 1.15.20",
		"- test 1.16: 1.16.19 → 1.16.21",
	))

	c, data := load(t, path)
	g.Expect(c.Minors).To(HaveLen(2), "patch mode never adds minors")
	g.Expect(c.Minors[0].Version).To(Equal("1.15.20"))
	g.Expect(c.Minors[1].Version).To(Equal("1.16.21"), "prereleases are ignored")
	g.Expect(data).To(HavePrefix("# A test catalog; this comment must survive."))
	g.Expect(data).To(ContainSubstring("valuesTemplate: |\n"), "block scalars stay block scalars")

	// Nothing left to do: the file is not touched.
	before := data
	changes, err = syncDir(dir, "patch")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(changes).To(BeEmpty())
	_, after := load(t, path)
	g.Expect(after).To(Equal(before))
}

func TestMinorMode(t *testing.T) {
	g := NewWithT(t)
	dir, path := setup(t, "1.15.19", "1.16.19", "1.17.0-pre.1", "1.17.2", "1.17.3", "1.18.0-rc.0", "2.0.1")

	changes, err := syncDir(dir, "minor")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(changes).To(HaveLen(1))
	g.Expect(changes[0]).To(ContainSubstring("test 1.17: new minor at 1.17.3"))

	c, _ := load(t, path)
	g.Expect(c.Minors).To(HaveLen(3), "1.18 has only prereleases and 2.0 is another major")
	added := c.Minors[2]
	g.Expect(added.Minor).To(Equal("1.17"))
	g.Expect(added.Version).To(Equal("1.17.3"))
	g.Expect(added.Kubernetes).To(Equal(">= 1.27.0-0, < 1.31.0-0"), "the range is copied for review")
	g.Expect(added.CompatibilitySource).To(Equal("https://docs.example.com/v1.17/compatibility/"))
}

// TestBuiltinCatalogsRoundTrip makes sure the sync tool rewrites the built-in
// catalogs without changing their layout, so its pull requests only show the
// lines that actually change.
func TestBuiltinCatalogsRoundTrip(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "catalog", "*.yaml"))
	NewWithT(t).Expect(err).ToNot(HaveOccurred())
	NewWithT(t).Expect(files).ToNot(BeEmpty())
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			g := NewWithT(t)
			data, err := os.ReadFile(f)
			g.Expect(err).ToNot(HaveOccurred())
			out, err := roundTrip(data)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(string(out)).To(Equal(string(data)), "run `go run ./hack/catalog-sync -mode=format` to normalize %s", f)
		})
	}
}

func TestChangedMinors(t *testing.T) {
	g := NewWithT(t)
	base, current := t.TempDir(), t.TempDir()
	write := func(dir, versions string) {
		data := fmt.Sprintf(fixtureCatalog, "https://charts.example.com")
		data = strings.Replace(data, "version: 1.16.19", versions, 1)
		g.Expect(os.WriteFile(filepath.Join(dir, "test.yaml"), []byte(data), 0o600)).To(Succeed())
	}
	write(base, "version: 1.16.19")
	write(current, "version: 1.16.20")

	out, err := changedMinors(base, current)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(Equal(`[{"cni":"test","minor":"1.16"}]`))

	out, err = changedMinors(current, current)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(Equal(`[]`))

	// A change outside the minors touches every minor.
	g.Expect(os.WriteFile(filepath.Join(current, "test.yaml"), []byte(strings.Replace(
		fmt.Sprintf(fixtureCatalog, "https://charts.example.com"), "image: {{ . }}/agent", "image: {{ . }}/agent-v2", 1)), 0o600)).To(Succeed())
	out, err = changedMinors(base, current)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(Equal(`[{"cni":"test","minor":"1.15"},{"cni":"test","minor":"1.16"}]`))

	out, err = changedMinors(filepath.Join(base, "missing"), current)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(Equal(`[{"cni":"test","minor":"1.15"},{"cni":"test","minor":"1.16"}]`), "every minor is new without a base")
}
