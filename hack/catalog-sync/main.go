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

// catalog-sync updates the catalog files from the upstream chart
// repositories.
//
// With -mode=patch it moves every minor to its newest released patch. With
// -mode=minor it appends a stub for every newer upstream minor, copying the
// previous minor's Kubernetes range for a reviewer to confirm against the
// linked compatibility page. Prereleases are ignored. Comments and layout of
// the catalog files are kept.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	yaml "go.yaml.in/yaml/v3"

	"github.com/vexxhost/cluster-api-addon-provider-cni/catalog"
	"github.com/vexxhost/cluster-api-addon-provider-cni/internal/charts"
)

func main() {
	dir := flag.String("catalog-dir", "catalog", "directory with the catalog files")
	mode := flag.String("mode", "patch", `"patch" bumps patch releases, "minor" adds new minors, "format" only normalizes the files`)
	summary := flag.String("summary-file", "", "file to write a markdown summary of the changes to")
	baseDir := flag.String("base-dir", "", `with -mode=changed, the directory with the catalog files to compare against`)
	flag.Parse()

	if *mode == "changed" {
		out, err := changedMinors(*baseDir, *dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(out)
		return
	}

	changes, err := syncDir(*dir, *mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	text := strings.Join(changes, "\n")
	fmt.Println(text)
	if *summary != "" {
		if err := os.WriteFile(*summary, []byte(text+"\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

// syncDir updates every catalog file in dir and returns a line per change.
func syncDir(dir, mode string) ([]string, error) {
	if mode != "patch" && mode != "minor" && mode != "format" {
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var changes []string
	for _, f := range files {
		c, err := syncFile(f, mode)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		changes = append(changes, c...)
	}
	return changes, nil
}

func syncFile(path, mode string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path comes from -catalog-dir
	if err != nil {
		return nil, err
	}
	if mode == "format" {
		out, err := roundTrip(data)
		if err != nil {
			return nil, err
		}
		return nil, os.WriteFile(path, out, 0o600) //nolint:gosec // the path comes from -catalog-dir
	}
	cat, err := catalog.Parse(data)
	if err != nil {
		return nil, err
	}
	repo, err := charts.LoadRepo(cat.Chart.RepoURL)
	if err != nil {
		return nil, err
	}
	latest := latestPatches(repo, cat.Chart.Name)

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	minors := mappingValue(doc.Content[0], "minors")
	if minors == nil || minors.Kind != yaml.SequenceNode || len(minors.Content) == 0 {
		return nil, fmt.Errorf("minors is not a list")
	}

	var changes []string
	switch mode {
	case "patch":
		for _, item := range minors.Content {
			minor, version := mappingValue(item, "minor"), mappingValue(item, "version")
			newest, ok := latest[minor.Value]
			if !ok {
				continue
			}
			current, err := semver.NewVersion(version.Value)
			if err != nil {
				return nil, err
			}
			if newest.GreaterThan(current) {
				changes = append(changes, fmt.Sprintf("- %s %s: %s → %s", cat.Type, minor.Value, version.Value, newest.Original()))
				version.Value = newest.Original()
			}
		}
	case "minor":
		last := minors.Content[len(minors.Content)-1]
		lastMinor, err := semver.StrictNewVersion(mappingValue(last, "minor").Value + ".0")
		if err != nil {
			return nil, err
		}
		for _, key := range sortedMinors(latest) {
			newest := latest[key]
			if newest.Major() != lastMinor.Major() || newest.Minor() <= lastMinor.Minor() {
				continue
			}
			minors.Content = append(minors.Content, stub(last, key, newest.Original()))
			changes = append(changes, fmt.Sprintf("- %s %s: new minor at %s; confirm its Kubernetes range at %s",
				cat.Type, key, newest.Original(), mappingValue(minors.Content[len(minors.Content)-1], "compatibilitySource").Value))
		}
	}
	if len(changes) == 0 {
		return nil, nil
	}

	out, err := encode(&doc)
	if err != nil {
		return nil, err
	}
	if _, err := catalog.Parse(out); err != nil {
		return nil, fmt.Errorf("the updated catalog is invalid: %w", err)
	}
	return changes, os.WriteFile(path, out, 0o600)
}

// roundTrip parses and re-encodes a catalog file the way the tool writes it.
func roundTrip(data []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return encode(&doc)
}

func encode(doc *yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// changedMinor is a catalog minor that is new or has a new version.
type changedMinor struct {
	CNI   string `json:"cni"`
	Minor string `json:"minor"`
}

// changedMinors returns, as a JSON list, every minor in dir that is missing
// from baseDir or differs there. When anything else in a catalog changes,
// such as its values template, every minor of that catalog counts as changed.
func changedMinors(baseDir, dir string) (string, error) {
	base, err := loadCatalogs(baseDir)
	if err != nil {
		return "", err
	}
	current, err := loadCatalogs(dir)
	if err != nil {
		return "", err
	}
	changed := []changedMinor{}
	for _, cniType := range sortedKeys(current) {
		cur, old := current[cniType], base[cniType]
		all := old == nil || !sameJSON(withoutMinors(cur), withoutMinors(old))
		for _, m := range cur.Minors {
			if all || !slices.ContainsFunc(old.Minors, func(o catalog.Minor) bool { return sameJSON(o, m) }) {
				changed = append(changed, changedMinor{CNI: cniType, Minor: m.Minor})
			}
		}
	}
	out, err := json.Marshal(changed)
	return string(out), err
}

// sameJSON compares the fields of two catalog values that come from the file.
func sameJSON(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// withoutMinors returns a catalog's settings other than its minors.
func withoutMinors(c *catalog.Catalog) catalog.Catalog {
	out := *c
	out.Minors = nil
	return out
}

// loadCatalogs loads the catalogs in dir. A missing directory has none.
func loadCatalogs(dir string) (catalog.Catalogs, error) {
	if dir == "" {
		return catalog.Catalogs{}, nil
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return catalog.Catalogs{}, nil
	}
	return catalog.Load(os.DirFS(dir))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// latestPatches maps every released minor of a chart to its newest patch.
func latestPatches(repo *charts.Repo, chart string) map[string]*semver.Version {
	latest := map[string]*semver.Version{}
	for _, cv := range repo.Index.Entries[chart] {
		v, err := semver.NewVersion(cv.Version)
		if err != nil || v.Prerelease() != "" {
			continue
		}
		key := fmt.Sprintf("%d.%d", v.Major(), v.Minor())
		if cur, ok := latest[key]; !ok || v.GreaterThan(cur) {
			latest[key] = v
		}
	}
	return latest
}

func sortedMinors(latest map[string]*semver.Version) []string {
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return latest[keys[i]].LessThan(latest[keys[j]]) })
	return keys
}

// stub copies the previous minor's entry for a new minor. The Kubernetes
// range is a placeholder for a reviewer to confirm.
func stub(previous *yaml.Node, minor, version string) *yaml.Node {
	prevMinor := mappingValue(previous, "minor").Value
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(key, value string, style yaml.Style) {
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: style})
	}
	add("minor", minor, yaml.DoubleQuotedStyle)
	add("version", version, 0)
	add("kubernetes", mappingValue(previous, "kubernetes").Value, yaml.DoubleQuotedStyle)
	if src := mappingValue(previous, "compatibilitySource"); src != nil {
		add("compatibilitySource", strings.ReplaceAll(src.Value, prevMinor, minor), 0)
	}
	return entry
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
