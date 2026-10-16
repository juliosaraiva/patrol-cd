// Package helm implements the Helm source transport and rendering helpers of the
// repo-server, re-implementing the behavior of the upstream baseline's util/helm
// on top of the Helm 4 CLI (run through internal/sandbox) and native HTTP/OCI
// clients.
//
// The package provides:
//   - [Client.ResolveChartVersion]: exact version pins and semver constraints
//     over an HTTP repository index (cached with TTL and ETag revalidation,
//     size-capped) or OCI registry tags;
//   - [Client.PullChart]: chart download (index URL or OCI chart layer) into a
//     credential-scoped archive cache and safe extraction into a fresh directory;
//   - [Client.Template]: `helm template` with the baseline argument order, value
//     precedence, comma escaping, api-versions error rewriting and Chart.lock
//     cleanup;
//   - [Client.DependencyBuild] / [Client.EnsureDependencies]: `helm dependency
//     build` with repositories and registry credentials written to per-call 0600
//     files;
//   - [Client.ShowChart], [Client.ShowValues], [Client.GetParameters]:
//     application-details helpers;
//   - [ResolveValueFilePathOrURL], [ValuesFileSchemes]: value-file path
//     confinement (internal/sandbox.SecureJoin) and remote scheme allow-list.
//
// Every helm subprocess gets an isolated home (HOME, XDG_*, HELM_*_HOME,
// HELM_REGISTRY_CONFIG, HELM_REPOSITORY_CONFIG/CACHE, HELM_CONTENT_CACHE,
// HELM_PLUGINS) that is deleted after the call. Credentials never appear in
// helm arguments or logs. The package does not import api/v1alpha1; callers map
// Repository/RepositoryCredential and ApplicationSource fields onto [Creds],
// [RepoCreds] and [TemplateOpts] (see docs/architecture/components/helm-oci.md).
package helm
