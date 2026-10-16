// Package render generates the Kubernetes manifests of one application source that has
// already been obtained on local disk (a git checkout, an extracted OCI artifact, a
// streamed local directory, or a Helm chart pulled by this package). It is the
// manifest-generation core of the repo-server (upstream reposerver/repository
// GenerateManifests, util/app/discovery, util/helm, util/kustomize and util/cmp).
//
// Renderer.Generate:
//
//  1. resolves the application path inside the root (or pulls a Helm chart source);
//  2. merges the in-repository parameter override files (.argocd-source.yaml,
//     .patrol-source.yaml, .argocd-source-<app>.yaml, .patrol-source-<app>.yaml, in
//     that order) into the source with RFC 7386 merge-patch semantics;
//  3. determines the tool: the explicit source block, else CMP discovery, else
//     Chart.yaml / kustomization files, else Directory, honoring the enabled source
//     types;
//  4. renders: Directory (YAML/JSON plus Jsonnet in a sandboxed child process),
//     Helm (internal/helm), Kustomize (edits applied to a scratch copy, never to the
//     shared checkout) or a Config Management Plugin sidecar (cmpwire protocol);
//  5. flattens List kinds, drops null lists and validates every object (apiVersion and
//     kind set, count and size limits).
//
// Tools run through internal/sandbox with an exact environment: the build environment
// (ARGOCD_APP_* and PATROL_APP_* variables) is exported to Kustomize and CMP and
// substituted into the fields upstream substitutes. Resource tracking metadata is not
// applied here (the controller applies it).
package render
