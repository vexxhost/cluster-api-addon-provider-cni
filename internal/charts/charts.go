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

// Package charts reads Helm chart repositories and renders charts offline
// with the Helm SDK. The catalog tests, the e2e fixtures and the catalog-sync
// tool use it; the controller itself leaves Helm to CAAPH.
package charts

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/repo"
)

// Repo is a downloaded chart repository index.
type Repo struct {
	URL   string
	Index *repo.IndexFile
}

// LoadRepo downloads the index of the HTTP chart repository at url.
func LoadRepo(url string) (*Repo, error) {
	dir, err := os.MkdirTemp("", "chart-index-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	r, err := repo.NewChartRepository(&repo.Entry{Name: "catalog", URL: url}, getter.All(cli.New()))
	if err != nil {
		return nil, err
	}
	r.CachePath = dir
	path, err := r.DownloadIndexFile()
	if err != nil {
		return nil, fmt.Errorf("downloading the index of %s: %w", url, err)
	}
	index, err := repo.LoadIndexFile(path)
	if err != nil {
		return nil, err
	}
	return &Repo{URL: url, Index: index}, nil
}

// Fetch returns a chart version from the repository. Downloads are cached in
// the user cache directory, since released charts never change.
func (r *Repo) Fetch(name, version string) (*chart.Chart, error) {
	cache := filepath.Join(cacheDir(), name+"-"+version+".tgz")
	if ch, err := loader.LoadFile(cache); err == nil {
		return ch, nil
	}

	cv, err := r.Index.Get(name, version)
	if err != nil {
		return nil, fmt.Errorf("%s %s in %s: %w", name, version, r.URL, err)
	}
	if len(cv.URLs) == 0 {
		return nil, fmt.Errorf("%s %s in %s has no download URL", name, version, r.URL)
	}
	u, err := repo.ResolveReferenceURL(r.URL, cv.URLs[0])
	if err != nil {
		return nil, err
	}
	g, err := getter.All(cli.New()).ByScheme("https")
	if err != nil {
		return nil, err
	}
	buf, err := g.Get(u)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", u, err)
	}
	data := buf.Bytes()
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err == nil {
		_ = os.WriteFile(cache, data, 0o600)
	}
	return loader.LoadArchive(bytes.NewReader(data))
}

// Render renders a chart like `helm template` would, validating the values
// against the chart's schema, and returns the manifest.
func Render(ch *chart.Chart, releaseName, namespace, kubeVersion string, values map[string]any) (string, error) {
	install := action.NewInstall(&action.Configuration{Log: func(string, ...any) {}})
	install.ClientOnly = true
	install.DryRun = true
	install.Replace = true
	install.IncludeCRDs = true
	install.ReleaseName = releaseName
	install.Namespace = namespace
	if kubeVersion != "" {
		kv, err := chartutil.ParseKubeVersion(kubeVersion)
		if err != nil {
			return "", err
		}
		install.KubeVersion = kv
	}
	rel, err := install.Run(ch, values)
	if err != nil {
		return "", err
	}
	return rel.Manifest, nil
}

func cacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "cluster-api-addon-provider-cni", "charts")
}
