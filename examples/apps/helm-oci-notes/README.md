# Helm charts and manifests from OCI registries

Two ways to consume content from an OCI registry. Both examples deploy the chart in
`../helm-chart/chart` after it was pushed; no binaries are stored here.

## 1. Push the chart

```bash
# Package the chart (version 0.1.0 from Chart.yaml) and push it as an OCI artifact.
helm package ../helm-chart/chart --destination /tmp/charts
helm registry login registry.example.com --username "$REGISTRY_USER" --password-stdin <<<"$REGISTRY_PASSWORD"
helm push /tmp/charts/guestbook-0.1.0.tgz oci://registry.example.com/charts
# -> registry.example.com/charts/guestbook:0.1.0
```

Plain manifests (no Helm) can be pushed as a single-layer OCI image, for example with ORAS
(run from a scratch directory; ORAS refuses absolute file paths by default):

```bash
tar -czf manifests.tar.gz -C path/to/examples/apps/plain-yaml/manifests .
oras push registry.example.com/manifests/guestbook:1.0.0 \
  manifests.tar.gz:application/vnd.oci.image.layer.v1.tar+gzip
```

Accepted layer media types are `application/vnd.oci.image.layer.v1.tar+gzip` and
`application/vnd.cncf.helm.chart.content.v1.tar+gzip`; more can be allowed with
`reposerver.oci.layer.media.types` in `patrol-cmd-params-cm`.

## 2. Register credentials

Private registries need a `Repository` (or a `RepositoryCredential` for every repository
below a URL prefix). See `examples/platform/repositories/oci-registry.yaml`:

| Style | Repository spec | Application source |
|-------|-----------------|--------------------|
| OCI source (`oci://`) | `type: oci`, `url: oci://registry.example.com/charts/guestbook` | `repoURL: oci://registry.example.com/charts/guestbook`, `path: .`, `targetRevision: 0.1.0` |
| Helm repository over OCI | `type: helm`, `enableOCI: true`, `url: registry.example.com/charts` (no scheme) | `repoURL: registry.example.com/charts`, `chart: guestbook`, `targetRevision: 0.1.0` |

## 3. Deploy

- `application-oci-source.yaml`: the artifact is pulled and its content rendered like a Git
  directory (here a Helm chart, so `path` must be `.`).
- `application-helm-oci.yaml`: Helm pulls the chart by name and version; `targetRevision`
  may be a semver range such as `0.1.*`.

`targetRevision` may also be a digest (`sha256:...`) to pin an immutable artifact.
