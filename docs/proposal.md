# CNI lifecycle for Cluster API

## Summary

A Cluster API add-on provider that installs, upgrades and takes over the CNI of workload clusters. A `ClusterCNI` per Cluster states which CNI to run and, optionally, which version. The controller picks a version that supports the cluster's Kubernetes version and upgrades along the CNI's supported upgrade path. Optional lifecycle hooks keep the CNI in step with each control plane step of a Kubernetes upgrade.

## Motivation

Every Cluster API user needs a CNI in every workload cluster, and Cluster API leaves it to them. The common approaches each stop short:

- A ClusterResourceSet with the `ApplyOnce` strategy installs the CNI and never touches it again.
- A ClusterResourceSet with the `Reconcile` strategy reapplies whatever manifest it holds, with no notion of upgrade order or compatibility.
- The [Cluster API Add-on Provider for Helm](https://github.com/kubernetes-sigs/cluster-api-addon-provider-helm) (CAAPH) installs and upgrades a chart to whatever version it is given, but knows nothing about which CNI version a Kubernetes version needs, which upgrades a CNI supports, or when to upgrade relative to the cluster.

Cluster API's [add-on orchestration proposal](https://github.com/kubernetes-sigs/cluster-api/blob/main/docs/proposals/20220712-cluster-api-addon-orchestration.md) planned a second iteration for exactly this: upgrading add-ons before, during or after a cluster upgrade, and choosing add-on versions to match the Kubernetes version. Only the first iteration, CAAPH, shipped. Meanwhile, Cluster API v1.12 added chained upgrades across several Kubernetes minor versions and lifecycle hooks for each step, which make the second iteration possible for the add-on that matters most.

### Goals

- Install a CNI on new clusters, choosing a version that supports the cluster's Kubernetes version.
- Upgrade the CNI along its supported path: to the latest patch of the running minor, then one minor at a time.
- Keep the CNI compatible through Kubernetes upgrades, including chained upgrades.
- Take over a CNI installed by a ClusterResourceSet or by hand, without disrupting it.
- Work with any Cluster API infrastructure, bootstrap and control plane provider, with or without ClusterClass.
- Keep the knowledge of CNI versions as reviewable data that stays current without manual work.

### Non-goals

- CNIs maintained outside this repository (v1). The catalog format allows adding them later.
- Managing CNI configuration beyond the generic fields; `providerConfig` passes chart values through.
- Replacing CAAPH. The controller uses it.

## Design

### API

```yaml
apiVersion: cni.vexxhost.com/v1alpha1
kind: ClusterCNI
metadata:
  name: my-cluster        # same name and namespace as the Cluster
spec:
  type: cilium            # cilium | calico, immutable
  version: ""             # "" newest compatible, "1.17" latest patch of a minor, "1.17.5" pinned
  kubeProxyReplacement: false
  podCIDRs: []            # defaults to the Cluster's pod CIDRs
  imageRepository: ""     # registry mirror
  chartRepository: ""     # chart repository mirror, immutable
  providerConfig: {}      # Helm values merged over the rendered ones
status:
  observedVersion: 1.16.19
  targetVersion: 1.18.17
  conditions:
    - type: Ready               # runs the target version
    - type: UpgradeCompatible   # running version supports the target and next Kubernetes versions
```

Naming the `ClusterCNI` after its Cluster makes one CNI per cluster a property of the API. The annotation `cni.vexxhost.com/next-kubernetes-version` announces the next Kubernetes version of an upgrade; the hooks set it, and any other orchestrator can.

### Catalog

Each CNI is one YAML file compiled into the controller:

- the chart and release to install, and labels for the release namespace;
- optionally, a separate chart with the CRDs some versions need first;
- how to detect an install made without the controller (a workload and container whose image tag is the version);
- a values template, rendered with the Cluster, the generic fields and the version;
- for every minor version: its latest patch, the Kubernetes versions upstream tests it with (with a link to the source), and optionally which versions it can be upgraded from.

The catalog is the only way to configure a CNI, and the controller and the hooks read nothing else. A daily workflow reads the upstream chart repositories with the Helm SDK: new patches are merged automatically once the end-to-end tests pass and released as a patch release of the provider, while new minors open a pull request for a person to confirm their Kubernetes range.

### Controller

On every change to the `ClusterCNI`, its Cluster or its HelmReleaseProxy, the controller:

1. waits for the control plane to be initialized;
2. reads the running Kubernetes version from the workload cluster, the desired version from the Cluster topology or the control plane's `spec.version`, and the announced next version;
3. finds the running CNI version: from its HelmReleaseProxy, or by detecting an existing install;
4. resolves the target version from `spec.version` and the Kubernetes versions, refusing downgrades;
5. picks the next step on the upgrade path: the detected version for a takeover, then the latest patch of the running minor, then the furthest allowed minor;
6. creates the release namespace with the labels the catalog asks for, such as the Pod Security level;
7. server-side applies the step's CRDs, from the chart's `crds/` directory (which Helm never upgrades) and from the catalog's CRD chart when the version has one (a Helm release can't hold Calico's, as it exceeds the size of the Secret Helm stores it in);
8. writes one HelmReleaseProxy per cluster for that step, with Helm's take-ownership option, and waits for CAAPH to deploy it.

Only the target version is checked against Kubernetes ranges. Intermediate steps follow the upgrade rule alone, so a cluster that already runs a CNI outside its tested range can still move forward.

The HelmReleaseProxy is owned by the Cluster: deleting a HelmReleaseProxy uninstalls the release, so deleting the `ClusterCNI` only stops management.

The controller reads only the core Cluster API contract: the Cluster, its control plane's `spec.version` and the kubeconfig Secret. It writes only its own status, the HelmReleaseProxy and the annotation.

### Lifecycle hooks

The manager also serves three Cluster API lifecycle hooks, registered with an `ExtensionConfig` that ships separately because Cluster API rejects it without the RuntimeSDK feature gate:

| Hook | Behavior |
|---|---|
| `BeforeClusterUpgrade` | Refuses an upgrade plan the CNI cannot follow, and waits while a CNI rollout is in progress. |
| `BeforeControlPlaneUpgrade` | Announces the step's version, then holds the step until the CNI runs a version that supports it. |
| `BeforeWorkersUpgrade` | Holds the workers until the CNI runs its target version. |

The hooks read the `ClusterCNI` and the catalog and return quickly; the controller does the work. Clusters without a `ClusterCNI` always pass.

### Taking over existing installs

A takeover installs exactly the running version with Helm's take-ownership option. With values that match the existing install, Helm only adds its ownership metadata, and the CNI's pods keep running. For Calico, the tigera-operator migrates a manifest-based install by itself when it runs the same Calico version. A ClusterResourceSet with the `Reconcile` strategy that applies the CNI would undo the takeover, so the controller waits until it no longer does.

### Consumers

A cluster manager or GitOps setup creates the `ClusterCNI` with the cluster and reads its conditions. For Kubernetes upgrades:

- with the RuntimeSDK feature gate and the `ExtensionConfig`, it only changes the Kubernetes version;
- without them, it sets the next-version annotation, waits for `UpgradeCompatible`, then changes the version.

## Alternatives

- **A HelmChartProxy per CNI.** CAAPH selects clusters by label and installs one chart version; it cannot step through versions per cluster or tie upgrades to the cluster's Kubernetes version, and moving a cluster between proxies reinstalls its CNI.
- **Embedding the Helm SDK.** CAAPH already handles chart repositories, credentials, kubeconfigs, rollout waits and take-ownership. Building on it keeps this provider small.
- **A controller per CNI.** The differences between CNIs are in their charts, versions and detection, all of which fit in data. One controller with a catalog keeps a single code path.
- **Holding workers through topology annotations.** Without hooks, `topology.cluster.x-k8s.io/hold-upgrade-sequence` could hold workers, but the controller would have to write into the user's Cluster spec. The next-version annotation keeps that decision with the consumer.

## Testing

- Unit tests cover the upgrade path, the catalog format and the hooks; every catalog minor is rendered with its real chart.
- Integration tests run the controller against envtest.
- End-to-end tests on kind and CAPD cover installing Cilium, upgrading it one minor at a time, a chained Kubernetes upgrade with Calico through the hooks, a cluster without ClusterClass, and taking over Cilium (also pulled from a registry mirror) and Calico installed by a ClusterResourceSet the way [magnum-cluster-api](https://github.com/vexxhost/magnum-cluster-api) does it.
- Every catalog change runs the end-to-end test for each minor it touches.
