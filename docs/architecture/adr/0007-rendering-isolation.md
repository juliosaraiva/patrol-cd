# ADR-0007: Rendering and tool isolation

Status: accepted — 2026-10-12

## Decision
- Git, Helm, Kustomize and git-lfs are invoked as pinned binaries bundled in the image
  (Helm 4.2.1, Kustomize 5.8.1 — the baseline's versions). Extra Kustomize versions are
  selected through `kustomize.path.<version>` settings, as upstream.
- Jsonnet is evaluated with go-jsonnet in a child process (`patrolcd jsonnet-eval`) so it can
  be time- and memory-limited and killed.
- `internal/sandbox` runs every tool with: sanitized env (allow-list), per-request temp dir,
  `Setpgid` + kill of the process group on timeout, CPU-time/address-space rlimits, stdout
  cap (`reposerver.max.combined.directory.manifests.size`-style limits), no inherited fds.
- Repository files are validated: symlinks resolving outside the repository root are
  rejected; archive extraction (Helm charts, OCI layers) rejects absolute paths, `..`,
  device files and enforces size/entry caps.
- Config Management Plugins run in sidecar containers that receive only the source tree and
  declared env, never repo credentials or kube tokens.
- NetworkPolicy restricts repo-server egress to configured repository hosts when
  `networkPolicy.repoServerEgress` is set (default: allow egress to 443/22/80 only).
