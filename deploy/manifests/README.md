# Patrol CD static install manifests

Generated from the Helm chart `deploy/helm/patrol-cd` by `hack/genmanifests` (`make
manifests-install`; `make verify-manifests` fails when they are stale). Do not edit the generated
files; change the chart or `profiles/` and regenerate. Guide:
[docs/guides/manifests.md](../../docs/guides/manifests.md).

| File | Kustomize base | Installation |
|------|----------------|--------------|
| `install.yaml` | `cluster-install/` | cluster-wide (API server and UI, controller with cluster-admin rights on the local cluster) |
| `namespace-install.yaml` | `namespace-install/` | namespace-scoped (Roles only; needs `crds.yaml` installed by a cluster administrator, and an installer allowed to grant the Roles' rights: a namespace `admin` is not; Applications on the local cluster need a Cluster object limited to the namespace; see the guide) |
| `core-install.yaml` | `core-install/` | headless (no API server, UI or notifications controller) |
| `ha/install.yaml`, `ha/namespace-install.yaml` | `ha/cluster-install/`, `ha/namespace-install/` | high availability (3 or more nodes) |
| `*-with-hydrator.yaml` | `*-with-hydrator/` | the same with the source hydrator (commit-server) |
| `crds.yaml` | `crds/` | CRDs and the Operation admission policy |
| — | `namespace/` | the namespace `patrol` with Pod Security labels |

Install (server-side apply is required: the ApplicationSet CRD is too large for client-side
apply):

```bash
kubectl apply --server-side --force-conflicts -f deploy/manifests/install.yaml
```

The manifests reference the unpublished image `patrol-cd/patrol-cd:0.1.0`: point them at your
registry with a kustomize overlay (`images:`, with `sortOptions: {order: fifo}` to keep the
apply order), see the guide. `profiles/` holds the chart value
files of each variant (inputs of the generator, hand-written).
