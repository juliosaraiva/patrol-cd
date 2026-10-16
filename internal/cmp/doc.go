// Package cmp is the Config Management Plugin sidecar server (`patrolcd cmp-server`).
//
// A plugin sidecar runs next to the repo-server with a plugin.yaml
// (ConfigManagementPlugin) and serves exactly one plugin on the unix socket
// "<socket dir>/<metadata.name>[-<spec.version>].sock" using the wire protocol of
// internal/render/cmpwire (ADR-0006). For every streamed request the server:
//
//  1. reads and validates the metadata frame (cmpwire.ReadMetadata), rejecting
//     archives above Config.MaxArchiveBytes before reading them;
//  2. extracts the gzip-compressed tar into a fresh per-request directory under the
//     work directory with internal/sandbox (bounded entry count and extracted size,
//     no absolute paths, '..' or out-of-bounds symlinks, execute bits stripped unless
//     spec.preserveFileMode), then verifies the payload size and SHA-256;
//  3. runs the configured commands (discover.find, init, generate, parameters.dynamic)
//     through sandbox.Run in the application directory, with an exact environment:
//     the plugin environment (sandbox.BaseEnv plus the sidecar variables allowed by
//     Config.PassEnv) followed by the request environment (later entries win). Nothing
//     else from the server process reaches a plugin;
//  4. removes the per-request directory.
//
// Each command is bounded by Config.Exec (default 90 s, PATROL_EXEC_TIMEOUT or
// ARGOCD_EXEC_TIMEOUT) and by the client deadline carried in cmpwire.HeaderDeadline,
// shortened by 100 ms; its process group receives SIGTERM and, after Config.KillGrace,
// SIGKILL.
//
// spec.provideGitCreds is parsed and reported by the config endpoint, but Patrol never
// forwards repository credentials to plugins (ADR-0007).
package cmp
