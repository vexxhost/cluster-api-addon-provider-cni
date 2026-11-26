# Contributing

Pull requests are welcome. Every commit needs a `Signed-off-by` line (`git commit -s`), certifying the [Developer Certificate of Origin](https://developercertificate.org).

## Development

You need Go and, for the end-to-end tests, Docker.

```sh
make generate   # deepcopy, CRD and RBAC from the kubebuilder markers
make test       # unit and envtest integration tests
make lint
make verify     # fails if generated files or go.mod are out of date
make test-e2e   # kind + CAPD, every spec; set GINKGO_FOCUS for one
```

The e2e specs are tagged by name: `[Cilium]`, `[Calico-Chained]`, `[KCP]`, `[Adoption-Cilium]`, `[Adoption-Cilium-Mirror]`, `[Adoption-Calico]` and `[Catalog]`. The mirror spec needs a registry on port 5000 of the host with the Cilium images, as set up in the e2e workflow, and `E2E_MIRROR=true`. The catalog spec needs `E2E_CATALOG_CNI` and `E2E_CATALOG_MINOR`, for example:

```sh
E2E_CATALOG_CNI=cilium E2E_CATALOG_MINOR=1.20 make test-e2e GINKGO_FOCUS='\[Catalog\]'
```

## The catalog

Each CNI is described by one file in [catalog/](catalog). The controller and the hooks only use these files, so supporting a new version or a new CNI is a change to data.

### Patch releases

The `catalog-sync` workflow checks the upstream chart repositories every day. New patch releases go into a `catalog/patch` pull request that merges by itself once the checks pass, and merging it tags a patch release of the provider.

### New minor versions

A new upstream minor version gets a `catalog/minor` pull request. Its entry copies the Kubernetes range of the previous minor, because neither upstream chart publishes one. Before approving it:

1. Open the `compatibilitySource` link of the new entry and set `kubernetes` to the versions listed there.
2. Check the upstream upgrade notes for values that changed. If the values template no longer fits, add a `valuesTemplate` to the new minor.
3. Wait for the `catalog` workflow, which installs the minor on the newest Kubernetes version in its range and upgrades to it from the previous minor.

`go run ./hack/catalog-sync -mode=minor` makes the same change locally.

### A new CNI

Add a catalog file with the chart, the release, how to detect an install made without this controller, a values template and the minors, then add the type to the `type` enum of the API. The catalog tests render every minor with its real chart, which catches values the chart rejects.

The values template sees the fields of [`catalog.RenderInput`](catalog/render.go): the Cluster as a generic map, the version, the pod and service networks, the control plane endpoint, the image mirror, kube-proxy replacement and whether an existing install is being taken over.
