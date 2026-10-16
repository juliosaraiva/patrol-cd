# Component: `internal/cmp` (Config Management Plugin sidecar server)

Status: implemented (library and entry point; `cmd/patrolcd cmp-server` wiring pending).
Decision records: [ADR-0006](../adr/0006-internal-apis.md) (CMP over a unix socket),
[ADR-0007](../adr/0007-rendering-isolation.md) (plugins receive only the source tree and
declared env, never repository credentials or kube tokens).

Upstream reference (behavior only, re-implemented): `cmpserver/plugin/{config,plugin}.go`,
`cmpserver/server.go`, `cmd/argocd-cmp-server`, `util/cmp/stream.go`,
`docs/operator-manual/config-management-plugins.md` (Argo CD v3.5.5).

## Responsibilities

1. Read and validate `plugin.yaml` (`ConfigManagementPlugin`) at startup.
2. Serve exactly one plugin on `<socket dir>/<metadata.name>[-<spec.version>].sock` using the
   `internal/render/cmpwire` protocol (imported, not redefined): `GET /plugin/v1/config`,
   `POST /plugin/v1/match|generate|parameters`.
3. For every streamed request: bound and receive the tar.gz, extract it with
   `internal/sandbox` into a fresh per-request directory, verify size and SHA-256, run the
   configured commands through `sandbox.Run` with an exact environment and timeouts, answer,
   and delete the directory.

The repo-server side (plugin selection, archiving, env construction) is `internal/render`
(see [render.md](render.md)).

## Migrating plugins from Argo CD

**Plugins do not inherit the sidecar's environment.** Upstream runs every plugin command
with the sidecar's whole process environment followed by the request environment
(`cmpserver/plugin/plugin.go`: `append(os.Environ(), ...)`; operator manual: "The system
environment variables of the sidecar"). Patrol passes only `PATH`, `HOME`, `TMPDIR`,
`LANG` plus the request environment (ADR-0007: sanitized allow-list env, so tokens and
service variables of the sidecar do not reach plugin code by accident).

Plugins that read configuration from the sidecar container's `env` (for example
argocd-vault-plugin's `AVP_*` and `VAULT_ADDR`, `HTTP(S)_PROXY`/`NO_PROXY`, `HELM_*`) see
those variables empty unless the sidecar sets `PATROL_CMP_PASS_ENV` (or `--pass-env`):

| Setting | Effect |
|---------|--------|
| `PATROL_CMP_PASS_ENV=*` | Upstream behavior: every sidecar variable, then the request env (request wins on duplicates). |
| `PATROL_CMP_PASS_ENV=AVP_*,VAULT_ADDR,HTTPS_PROXY,NO_PROXY` | Recommended: only the named variables / `PREFIX*` groups. |
| unset (default) | `PATH`, `HOME`, `TMPDIR`, `LANG` only. |

At startup the server logs which mode is active. The request environment (build
variables, `ARGOCD_ENV_*`, `PARAM_*`) is applied after the plugin environment in every mode,
as upstream. Parity status: SRC-078 is `different-by-design` for this default.

## Public API

| Symbol | Purpose |
|--------|---------|
| `ReadPluginConfig(dir)`, `ParsePluginConfig(data)` | Parse `<dir>/plugin.yaml` (≤ 1 MiB) and validate. Errors keep upstream's messages: `invalid plugin configuration file. metadata.name should be non-empty`, `... kind should be ConfigManagementPlugin, found <k>`, `... spec.generate command should be non-empty`; Patrol adds: name/version must be a file-name component (no `/`, `\`, NUL, `.`, `..`), `command[0]` non-empty, no NUL in arguments. |
| `PluginConfig{APIVersion, Kind, Metadata{Name, Namespace, Labels, Annotations}, Spec}` | `Spec{Version, Init, Generate, Discover{FileName, Find{Command, Args, Glob}}, Parameters{Static []apiclient.ParameterAnnouncement, Dynamic}, PreserveFileMode, ProvideGitCreds, LockRepo}`; `Command{Command, Args}` runs `Command[0]` with `Command[1:]+Args`. Methods: `Validate`, `Warnings`, `PluginName` (`<name>[-<version>]`), `SocketName`, `IsDiscoveryConfigured`. |
| `New(Config) (*Server, error)` | Validates, removes and recreates `<WorkDir>/_cmp_server` (0700). `Config{Plugin, SocketDir, SocketMode, WorkDir, Exec sandbox.Limits, KillGrace, PassEnv, Environ, MaxArchiveBytes, Archive sandbox.ArchiveLimits, ReceiveTimeout, ShutdownTimeout, Registerer, Logger}`. |
| `(*Server).ListenAndServe(ctx)` | Creates the socket dir if missing, removes a stale socket (refuses a live one or a non-socket file), listens, `chmod`s the socket to `SocketMode`, serves until `ctx` is done, removes the socket. |
| `(*Server).Serve(ctx, net.Listener)`, `Handler()`, `SocketPath()`, `WorkDir()` | Serving on a caller-provided listener; the bare handler for embedding/tests. |
| `Options`, `DefaultOptions()`, `OptionsFromEnv(lookup)`, `(*Options).AddFlags(*pflag.FlagSet)`, `Options.Validate/NewLogger/Config` | Process settings for `patrolcd cmp-server` (see below). |
| `Run(ctx, Options, *slog.Logger, prometheus.Registerer)` | Entry point: validate options, read plugin.yaml (startup fails on the validation errors), log warnings, `New`, `ListenAndServe`. The caller maps SIGINT/SIGTERM to `ctx` cancellation (`signal.NotifyContext`). |
| `CommandError{Args, Cause, Stderr}` | Failed command: ``"`<args>` failed <cause>: <stderr>"`` (upstream shape); unwraps to the `sandbox` error. |

Defaults: socket dir `/home/patrol/cmp-server/plugins` (`cmpwire.DefaultSocketDir`), config
dir `/home/patrol/cmp-server/config`, work dir parent `os.TempDir()`, socket mode `0600`,
exec timeout 90 s, kill grace 5 s, receive timeout 5 min, stdout cap 256 MiB, compressed
archive ≤ 1 GiB, extraction ≤ 1 GiB total / 1 GiB per file / 500 000 entries, shutdown
timeout 20 s.

### Process settings (`Options`)

| Flag | Environment (Patrol, then upstream name) | Default |
|------|-------------------------------------------|---------|
| `--config-dir-path` | — | `/home/patrol/cmp-server/config` |
| `--socket-dir` | `PATROL_PLUGINSOCKFILEPATH`, `ARGOCD_PLUGINSOCKFILEPATH` | `/home/patrol/cmp-server/plugins` |
| `--work-dir` | `PATROL_CMP_WORKDIR`, `ARGOCD_CMP_WORKDIR` | `os.TempDir()` (+ `/_cmp_server`) |
| `--exec-timeout` | `PATROL_EXEC_TIMEOUT`, `ARGOCD_EXEC_TIMEOUT` (Go duration) | `90s` |
| `--pass-env` | `PATROL_CMP_PASS_ENV` (`,`/`;`/space separated; `*` = upstream behavior, see [Migrating plugins](#migrating-plugins-from-argo-cd)) | none |
| `--receive-timeout` | `PATROL_CMP_RECEIVE_TIMEOUT` (Go duration, `0` = client deadline only) | `5m` |
| `--max-archive-bytes` | `PATROL_CMP_MAX_ARCHIVE_SIZE` (quantity, `0` = unlimited) | 1 GiB |
| `--max-extracted-bytes` | `PATROL_CMP_MAX_EXTRACTED_SIZE` (quantity, `0` = unlimited) | 1 GiB |
| `--max-files` | `PATROL_CMP_MAX_FILES` | 500 000 |
| `--loglevel`, `--logformat` | `PATROL_CMP_SERVER_LOGLEVEL/_LOGFORMAT`, `ARGOCD_CMP_SERVER_LOGLEVEL/_LOGFORMAT` | `info`, `json` |

Invalid values fail startup (upstream falls back to defaults with a warning for durations).

## Behavior

### plugin.yaml (SRC-069)

`kind: ConfigManagementPlugin` is required. `apiVersion` is accepted as
`argoproj.io/v1alpha1` (compatibility) or `cd.patrol.example/v1alpha1`; other values are
accepted with a warning (upstream does not validate it). Unknown fields are ignored with a
warning (strict re-parse), `lockRepo` (removed v1 field) is accepted and ignored with a
warning, `provideGitCreds: true` logs a warning. Without discovery rules the upstream info
message tells users to set `spec.source.plugin.name` to `<name>[-<version>]`. Configuration
changes need a restart.

### Socket and lifecycle (SRC-070)

Socket `<dir>/<name>-<version>.sock` (or `<name>.sock`). A stale socket file (no listener) is
removed before listening; a socket that still accepts connections (duplicate name/version) or
a non-socket file stops startup instead of being clobbered. On shutdown the listener closes
(socket unlinked) immediately, in-flight requests get `ShutdownTimeout`, then their contexts
are cancelled (process groups terminated) and their directories removed before `Serve`
returns.

### Request handling (SRC-085, SRC-089)

1. Method/media type checks (`405`/`415`, problem+json). `Patrol-Cmp-Deadline` (RFC 3339) sets
   the request deadline minus 100 ms (upstream `cmpTimeoutBuffer`); malformed → `400`.
   The receive phase (steps 2–3) ends at that deadline or `ReceiveTimeout` after the request
   started, whichever is earlier: the context stops extraction between entries and the
   connection read deadline interrupts a read blocked on a client that stopped sending
   (upstream: the gRPC stream deadline). A stalled stream is answered with `504
   deadline_exceeded` (`... error receiving stream: timed out receiving the request
   stream`), its directory removed and the connection closed. The read deadline is lifted
   once the body is consumed, so commands are bounded by the exec timeout and the request
   deadline, not by the receive deadline.
2. Body capped at `4 + cmpwire.MaxMetadataBytes + MaxArchiveBytes`; `cmpwire.ReadMetadata`
   (`error getting stream metadata: metadata is nil`, invalid metadata, `illegal appPath: out
   of workDir bound`); `metadata.size > MaxArchiveBytes` → `413 resource_exhausted` before any
   payload is read.
3. `os.MkdirTemp(<work>/_cmp_server, "req-")` (0700, unique per request) and
   `sandbox.ExtractTarGzContext` through `cmpwire.PayloadReader`: absolute names, `..`,
   out-of-bounds or absolute symlinks, devices/fifos are rejected (`400 invalid_argument`);
   entry count, per-file and total extracted bytes are bounded (decompression bombs →
   `413 resource_exhausted`). Execute bits are stripped unless `spec.preserveFileMode`
   (SRC-083). The rest of the payload is then drained (bounded by `metadata.size` and the
   receive deadline; skipped once that has passed, which fails the request) so a tampered
   stream reports `file checksum validation error` (or the size mismatch) rather than an
   archive error.
4. The application directory is `sandbox.SecureJoin(<request dir>, appRelPath)`; commands
   require it to be a directory (`application path "<p>" is not a directory of the received
   source`, `400`).
5. After the response the request directory is removed through `os.Root` (no symlink
   following); directories a plugin made unwritable are made owner-accessible and removal is
   retried; failures are logged with `security=3`, `CWE=459` (upstream fields).

Error messages keep upstream's prefixes: `match repository error receiving stream: ...`,
`match repository error: ...`, `generate manifest error receiving stream: ...`, `error
generating manifests: ...`, `parameters announcement error receiving stream: ...`, `get
parameters announcement error: ...`. The per-request directory prefix is stripped from
messages (files are named relative to the archive root).

Status/codes (`apiclient.Code*`): malformed input `400 invalid_argument`; limits
`413 resource_exhausted`; command timeout, request deadline or receive deadline
`504 deadline_exceeded`;
stdout cap `500 resource_exhausted`; command failures and unparsable output
`500 internal`; unknown endpoint `404 not_found`.

### Commands and environment (SRC-074, SRC-076..078, SRC-088)

Every command (`discover.find`, `init`, `generate`, `parameters.dynamic`) runs through
`sandbox.Run` in the application directory with:

- **Environment** = plugin environment + request environment (`metadata.env`, which carries
  `ARGOCD_APP_*`/`PATROL_APP_*`, `KUBE_VERSION`, `KUBE_API_VERSIONS`, `ARGOCD_ENV_*`,
  `ARGOCD_APP_PARAMETERS`, `PARAM_*` built by the repo-server), one entry per name, later
  entries win (exec semantics). The plugin environment is `sandbox.BaseEnv` (`PATH`, `HOME`,
  `TMPDIR`, `LANG=C.UTF-8`) plus the sidecar variables matching `PassEnv` (exact names,
  `PREFIX*`, or `*` for upstream's "all sidecar variables"). Nothing else from the server
  process reaches a plugin (a deliberate difference, see
  [Migrating plugins from Argo CD](#migrating-plugins-from-argo-cd)).
- **Executable**: absolute paths as is; relative paths with a separator (`./gen.sh`) relative
  to the application directory (as `os/exec` with `Dir`); bare names looked up in the plugin
  environment's `PATH` (absolute entries only), never in the request's.
- **Limits**: `Exec.Timeout` (90 s; `PATROL_EXEC_TIMEOUT`/`ARGOCD_EXEC_TIMEOUT`) per command
  and the request deadline; on either, the process group gets SIGTERM and SIGKILL after
  `KillGrace` (5 s); grandchildren are killed when the leader exits. Stdout ≤ 256 MiB,
  stderr ≤ 16 MiB, stdin `/dev/null`, no inherited descriptors.
- **Errors**: ``"`<args>` failed exit status N: <stderr tail ≤ 4 KiB>"``; timeouts read
  `failed exec timeout exceeded` / `failed request deadline exceeded`.
- One trailing newline is trimmed from stdout. Empty output logs a warning
  (`Plugin command returned zero output`).

### Discovery — `POST /plugin/v1/match` (SRC-072..074)

First rule set wins: `discover.fileName` (`filepath.Glob` of `<app>/<fileName>`, no `**`),
else `discover.find.glob` (`**` supported via doublestar, relative to the app directory,
symlinks not followed during traversal; patterns leaving the app directory are an error),
else `discover.find.command` (supported iff exit 0 and non-empty stdout). Errors: `error
finding filename match for pattern "<p>": ...`, `error finding glob match for pattern "<p>":
...`, `error running find command: ...`. Without rules: `{isSupported: false,
isDiscoveryEnabled: false}`.

### Generate — `POST /plugin/v1/generate` (SRC-076, SRC-077)

`init` (stdout ignored, failure aborts) then `generate`; stdout is split as a YAML or JSON
object stream (k8s `YAMLOrJSONDecoder`), empty and `null` documents dropped, each document
returned as JSON (`failed to unmarshal manifest: ...` otherwise). Plugin output is never
logged (sizes only); stderr is logged at debug level only, bounded to 4 KiB.

### Parameters — `POST /plugin/v1/parameters` (SRC-081, SRC-082)

Dynamic announcements (stdout of `parameters.dynamic`, a JSON array) followed by the static
ones; errors `error executing dynamic parameter output command: ...` and `error unmarshaling
dynamic parameter output into ParametersAnnouncementResponse: ...`. The response list is
never null.

### Config — `GET /plugin/v1/config` (SRC-070, SRC-084)

`{isDiscoveryConfigured, provideGitCreds}`; `provideGitCreds` mirrors plugin.yaml so the
repo-server can warn; credentials are never forwarded.

### Metrics and logging

Registered on `Config.Registerer` with const label `plugin=<name>[-<version>]`:
`patrol_cmp_requests_total{operation=config|match|generate|parameters, outcome=success|<code>}`,
`patrol_cmp_request_duration_seconds{operation}`, `patrol_cmp_requests_in_flight`. Logs
(`log/slog`, `component=cmp-server`): one line per request (operation, app name, app path,
archive bytes, duration, manifest count or error code/message with plugin stderr omitted)
and per command (command base name, argument count, duration, output sizes). Request
environment values, plugin output and stderr are not logged above debug.

## Differences from upstream

- Transport: `cmpwire` HTTP/1.1 over the unix socket instead of gRPC streams (ADR-0006); no
  gRPC health/reflection/version services; the socket is `0600` by default.
- Environment (SRC-078, `different-by-design`): plugins do not inherit the sidecar
  environment; only `PATH`, `HOME`, `TMPDIR`, `LANG` and variables allowed by
  `PATROL_CMP_PASS_ENV` (`*` restores upstream). See
  [Migrating plugins from Argo CD](#migrating-plugins-from-argo-cd).
- Reception is bounded by `ReceiveTimeout` (5 min) even without a client deadline; upstream
  bounds it only by the gRPC deadline the client sets.
- Per-command exec timeout (`PATROL_EXEC_TIMEOUT`/`ARGOCD_EXEC_TIMEOUT`, 90 s) is enforced
  by the server (upstream documents it but the cmp-server code only honors the request
  deadline).
- Archives are bounded (compressed size, extracted size, entry count) and extraction is
  stricter (`sandbox`): upstream extracts without a size limit.
- `provideGitCreds` (SRC-084): never supported; plugins never receive repository
  credentials (ADR-0007). The flag is reported and warned about.
- A live socket with the same name is not clobbered; startup fails instead.
- `apiVersion` other than the two known values and unknown fields produce warnings.
- `discover.find.glob` patterns may not leave the application directory.
- Invalid duration/size settings fail startup instead of falling back to defaults.
- Plugin stderr is logged at debug only (upstream logs it at info on success).

## Requests to other components

- `cmd/patrolcd`: add the `cmp-server` subcommand: `o, err := cmp.OptionsFromEnv(os.LookupEnv)`,
  `o.AddFlags(cmd.Flags())`, `logger, _ := o.NewLogger(os.Stderr)`,
  `cmp.Run(signal.NotifyContext(ctx, SIGINT, SIGTERM), o, logger, registry)`; call
  `sandbox.MaybeHelperMain()` first in `main` (needed if rlimits are configured).
- `deploy/` (Helm chart): sidecars share the socket `emptyDir` with the repo-server, run as
  the repo-server's UID (socket mode `0600`), mount a dedicated `/tmp` (not shared with the
  repo-server), no service account token, no Secrets.

## Tests

`go test -race ./internal/cmp/` — real unix sockets, real `sh`/`find`/`sleep`, and the
repo-server's CMP client (`internal/render`) for end-to-end paths. Covered: plugin.yaml parsing
(upstream example, validation errors, warnings, size bound), socket naming/permissions/stale
and live sockets/removal, config endpoint, discovery by fileName/glob/command (match, no
match, failure, bad patterns, precedence, env), auto-discovery across three sidecars, generate
with init and the full request env, YAML/JSON/empty/invalid output, init and generate
failures, parameters static+dynamic order and errors, `preserveFileMode`, exec timeout and
client deadline (process group killed, verified by PID), stalled streams (payload or metadata,
with a client deadline or `ReceiveTimeout`, and the post-error drain) answered `504`/`400` at
the deadline with the directory removed, commands outliving the receive deadline on a reused
connection, tar bomb / entry count / archive size
/ `..` / absolute / symlink / device / checksum / truncation rejection, env isolation (injected
and real process env), concurrent isolated requests and cleanup (including unwritable
directories), `PATROL_CMP_PASS_ENV=*` (sidecar variables visible, request env wins), work-dir
wipe at startup, graceful and forced shutdown, metrics, options from
env/flags, `Run` end to end and startup validation failures; rapid properties for env merging
and manifest splitting.
