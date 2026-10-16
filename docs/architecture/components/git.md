# Component: `internal/git`

Status: implemented (phase 1, library only). Decision records: [ADR-0007](../adr/0007-rendering-isolation.md)
(tool isolation), [ADR-0006](../adr/0006-internal-apis.md) (repo-server and commit-server use it).

`internal/git` is the Git transport of the repo-server (fetch, checkout, revision metadata,
signature checks), the controller (revision resolution through repo-server) and the
commit-server (source hydrator writes). It drives the `git` CLI through
`internal/sandbox.Run` and mirrors the observable behavior of upstream Argo CD v3.5.5
`util/git` (plus `util/versions` semver resolution and `reposerver/repository/lock.go`).
It does not import `api/v1alpha1`; callers adapt CRD types (see "Adapting configuration").

## Responsibilities

1. Credential delivery to git without putting secrets on the command line or in persistent
   files: HTTPS username/password through a `GIT_ASKPASS` helper (the current executable
   re-executed in helper mode), bearer / forced basic headers through
   `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`, SSH keys / known hosts / TLS
   material as 0600 files in a per-command 0700 directory that is removed after the command.
2. Remote ref listing (`ls-remote --symref`), with an optional shared TTL cache that
   coalesces concurrent listings, and revision resolution (HEAD, branches, tags, peeled
   annotated tags, any `refs/...`, semver constraints with `tagPrefix`, full and truncated
   SHAs).
3. A persistent working copy per repository: init, fetch (full or shallow, LFS), forced
   checkout with reset + `clean -ffdx`, submodules, upstream's "fetch only when missing,
   fall back to FETCH_HEAD" checkout strategy, changed files between commits.
4. Commit metadata (author, date, tags, message, `Argocd-reference-commit-*` trailers) and
   GnuPG verification of commits and annotated tags (`verify-commit --raw` /
   `verify-tag --raw` with a caller-provided `GNUPGHOME`).
5. Hydrator write operations: author identity, checkout-or-orphan / checkout-or-new
   branches, remove paths, commit everything (message via stdin, optional signoff), push
   (optionally forced), git notes add (fetch + retry on concurrent update) and read.
6. URL helpers (`NormalizeURL`, `SameURL`, `IsSSHURL`, `IsHTTPSURL`, `RepoKey`,
   `SanitizeURL`, `SSHHostWithPort`) and `RepoLock`, the per-working-copy
   reference-counted lock.

## Public API

| Symbol | Purpose |
|--------|---------|
| `Creds` interface `Environ(workdir) (env, cleanup, err)` | Credential environment for one command; `workdir` is a private per-command directory. |
| `HTTPSCreds{Username, Password, BearerToken, ClientCertData, ClientCertKey, CAData, Insecure, ForceBasicAuth}` | HTTPS auth. Username defaults to `x-access-token`. Header precedence: forced basic (needs password) > bearer. Client cert only when both cert and key are set. `CAData` replaces system roots (`GIT_SSL_CAINFO`); `Insecure` sets `GIT_SSL_NO_VERIFY` and ignores `CAData`. |
| `SSHCreds{PrivateKey, InsecureIgnoreHostKey, KnownHosts, Proxy}` | `GIT_SSH_COMMAND="ssh -i <key> -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=<file>"`; insecure: `StrictHostKeyChecking=no`, `UserKnownHostsFile=/dev/null` plus a warning log. `Proxy` (`socks5`/`socks5h` with host and port) goes through a `connect-proxy` wrapper script; URL credentials become `SOCKS5_USER`/`SOCKS5_PASSWD`. |
| `NopCreds{}` | Anonymous access. |
| `ValidateSSHProxyURL`, `ValidateHTTPProxyURL` | Upstream proxy URL safety rules, for callers that drop unsafe proxies at construction. |
| `MaybeAskpassMain()`, `SetAskpassPath(p)` | Askpass helper entry point (must be the first call in `main` of `cmd/patrolcd` and in `TestMain` of packages using HTTPS creds); override of the helper binary. |
| `New(repoURL, root, creds, opts...) (*Client, error)` | Client for one URL and one working copy (`root` may be empty for list/resolve-only clients). |
| Options | `Insecure`, `EnableLFS`, `Proxy`, `NoProxy`, `Depth`, `Timeout`, `KillGrace`, `GitPath`, `SubmodulesEnabled`, `TagPrefix`, `WithRefCache(cache, read)`, `GnuPGHome`, `GPGProgram`, `Retry(attempts, base, max)`, `BuiltinGitConfig`, `CredentialsDir`, `AllowLocalRepositories`, `WithLogger`. |
| `(*Client).Init / Fetch / Checkout / CheckoutRevision / Submodule` | Working copy management (see "Checkout and fetch"). |
| `(*Client).LsRemote(ctx) (Refs, error)` | `Refs{Branches, Tags []string (sorted short names); HEAD (sha); HEADRef}`. |
| `(*Client).ResolveRevision(ctx, rev) (sha, error)` | Upstream `LsRemote` semantics plus truncated-SHA expansion. |
| `(*Client).CommitSHA / IsRevisionPresent / ChangedFiles` | `rev-parse HEAD`; `cat-file -t == commit`; `diff --name-only --no-renames -z a..b`. |
| `(*Client).RevisionMetadata(ctx, rev) (*RevisionMetadata, error)` | `Author`, `Date`, `Tags`, `Message`, `References []RevisionReference{Commit *CommitMetadata}`, `Signature *SignatureInfo` (with `GnuPGHome`). |
| `ParseReferences(logger, trailers)` | Upstream `GetReferences` (also used by the hydrator for dry-commit metadata). |
| `(*Client).VerifyCommitSignature(ctx, rev, gnupgHome) (keyID, error)` / `VerifySignature(...) (SignatureInfo, error)` | GnuPG verification; `SignatureInfo{Result, KeyID, Fingerprint, PrimaryFingerprint, Signer, Trust}`. |
| `(*Client).SetAuthor / CheckoutOrOrphan / CheckoutOrNew / RemoveContents / CommitAll / Push / AddNote / GetNote` | Hydrator writes. |
| `MaxVersion(rev, tags, prefix)`, `IsConstraint(rev)` | Upstream `util/versions` semantics (Masterminds semver v3). |
| `RefCache`: `NewRefCache(ttl, max)`, `Invalidate(url)`, `Len()` | Shared ref listing cache (default TTL 3m, 1024 entries). |
| `RepoLock.Lock(ctx, path, revision, allowConcurrent, init) (io.Closer, error)`, `NopCloser` | Upstream `repositoryLock` with context-aware waiting. |
| URL helpers | `NormalizeURL`, `SameURL`, `IsSSHURL`, `IsHTTPSURL`, `IsHTTPURL`, `RepoKey`, `SanitizeURL`, `SSHHostWithPort`, `IsCommitSHA`, `IsTruncatedCommitSHA`. |
| Errors | `ErrInvalidRevision`, `ErrRevisionNotFound` (`*RevisionNotFoundError`, message `unable to resolve '<rev>' to a commit SHA`), `ErrAuthentication`, `ErrNothingToCommit`, `ErrNoteNotFound`, `ErrUnsigned`, `ErrSignatureInvalid` (`*SignatureError`), `ErrInvalidURL`, `ErrInvalidCredentials`, `ErrNotInitialized`, `*CommandError{Op, Message, Kind, Err}` (unwraps to both `Kind` and the sandbox error, e.g. `sandbox.ErrTimeout`, `*sandbox.ExitError`, `context.Canceled`). |

## How commands run

Every command is a `sandbox.Run` with:

- an exact environment: `PATH` (process PATH), `HOME=/dev/null`, `TMPDIR`, `LANG=C`,
  `LC_ALL=C`, `GIT_TERMINAL_PROMPT=0`, `GIT_LFS_SKIP_SMUDGE=1`, `GIT_CONFIG_GLOBAL=/dev/null`,
  `GIT_CEILING_DIRECTORIES=<parent of the working directory>` (git never discovers an
  enclosing repository), proxy variables (from `Proxy`/`NoProxy`, otherwise the process'
  `http_proxy`/`https_proxy`/`no_proxy`/`all_proxy` family is passed through, as upstream
  inherits it), `SSL_CERT_FILE`/`SSL_CERT_DIR` when set;
- configuration through `GIT_CONFIG_COUNT`: `maintenance.autoDetach=false` and
  `gc.autoDetach=false` (upstream built-in config, `BuiltinGitConfig(false)` disables),
  `credential.helper=` (no system credential helpers), `core.hooksPath=/dev/null`,
  `core.fsmonitor=false`, `protocol.ext.allow=never`, `safe.bareRepository=explicit`,
  `gpg.program` when `GPGProgram` is set, followed by entries from `Creds` (unscoped
  `http.extraHeader` keys are rewritten to `http.<origin>/.extraHeader`, or dropped when
  the repository has no http(s) origin; every header scope gets
  `http.<scope>.followRedirects=false`, see "Security notes");
- `Timeout` (default `sandbox.DefaultTimeout`, 90 s) and `KillGrace` (default 10 s:
  SIGTERM, then SIGKILL — upstream's git behavior), process-group kill, output caps
  (1 MiB for small plumbing output, 16 MiB for metadata, 64 MiB for `ls-remote`,
  256 MiB default);
- `Cmd.Redact` with every credential secret (password, token, client key, base64 basic
  credentials, URL password and its escaped forms), longest first.

Credentialed commands (`ls-remote`, `fetch`, `push`, `submodule`, `lfs fetch/pull`) get a
fresh `MkdirTemp` directory (0700, under `CredentialsDir`, default `os.TempDir()`); `Creds`
writes its files there, the client adds `PATROL_GIT_ASKPASS_ORIGIN=<scheme://host[:port]>`,
and the directory is removed after the command. `ls-remote` runs inside that directory, not
in the working copy.

Askpass helper: git runs `GIT_ASKPASS` (the current executable) with the prompt as the only
argument. `MaybeAskpassMain` answers `Username for '<url>'` / `Password for '<url>'`
prompts (single or double quotes) only when the prompt URL has the same scheme, host and
port as `PATROL_GIT_ASKPASS_ORIGIN`; it reads the JSON file named by
`PATROL_GIT_ASKPASS_FILE` (absolute path, regular file, no group/other permissions,
≤ 64 KiB) and refuses answers containing line breaks.

A password embedded in an http(s) URL is removed from the URL used for git (argv and
`.git/config`) and delivered through askpass (as `HTTPSCreds{Username, Password}` when no
other credentials were given).

## Revision resolution

`ResolveRevision` mirrors upstream `nativeGitClient.lsRemote`:

1. A full 40-hex (or 64-hex) SHA is returned unchanged without contacting the remote.
2. The ref listing is obtained (`git ls-remote --symref -- <url>`, tags replaced by their
   peeled `^{}` commit, listing order kept).
3. `""` becomes `HEAD`; `MaxVersion(rev, tags, TagPrefix)` replaces a semver constraint by
   the highest matching tag (errors are ignored and the revision is used literally).
4. The first ref whose go-git short name equals the revision (when the revision is itself
   a short name) or whose full name equals it wins; symbolic refs are followed.
5. A 7+ hex string is a truncated SHA: it is expanded in the working copy (`Init`,
   `rev-parse --verify <rev>^{commit}`, `Fetch("")` and retry when absent). Upstream
   returns truncated SHAs unchanged; Patrol always returns a full SHA.
6. Otherwise `*RevisionNotFoundError`.

`RefCache` keys listings by `RepoKey(url)` (so URLs that may name different repositories
never share a listing) plus a process-local HMAC of the credentials; `Invalidate(url)`
drops the listings of every spelling `SameURL` considers equal;
concurrent misses for a key run one `ls-remote` (singleflight, not cancelled by the first
caller's context); `WithRefCache(cache, false)` skips reads but refreshes the entry
(upstream `noRevisionCache`). Credential types outside this package bypass the cache.

## Checkout and fetch

- `Init`: keeps an existing repository (`.git/HEAD` present) whose `remote.origin.url` is
  the client's URL; another spelling of the same location (same `RepoKey`) only gets
  `remote.origin.url` replaced; a repository whose origin is missing or another location
  is discarded. Otherwise `Init` empties `root`, runs `git init --template=` and sets
  `remote.origin.url` / `remote.origin.fetch` (`+refs/heads/*:refs/remotes/origin/*`).
  A failed init removes the partial `.git`.
- `Fetch(rev)`: `fetch --force --prune [--depth N | --tags] -- origin [rev]`, credentialed,
  where `rev` must be `HEAD`, an object name or a ref name (`check-ref-format` rules);
  refspec syntax (`src:dst`, leading `+` or `^`, globs) and revision expressions (`~`,
  `^`, `@{`, `..`) are `ErrInvalidRevision`, so a revision never updates local refs;
  retried on transient transport errors when `Retry` is configured; temp pack files older
  than 2×timeout are removed after a failure; with LFS, `lfs fetch --all` when the current
  checkout has LFS files.
- `Checkout(rev, submodules)`: `""`/`HEAD` → `origin/HEAD`; `checkout --force <rev> --`,
  `reset --hard HEAD`, LFS (`lfs ls-files -n`, then `lfs pull`), submodules when
  `submodules && SubmodulesEnabled && .gitmodules exists` (`submodule sync --recursive`,
  `submodule update --init --recursive`, credentialed), then always `clean -ffdx`.
- `CheckoutRevision(rev, submodules)`: validates `rev` with the `Fetch` rule (`""` allowed)
  before running anything; then upstream `checkoutRevision` — `Init`; fetch only if
  `IsRevisionPresent` is false (the revision itself at `Depth`, otherwise the default
  refspec); `Checkout`; on failure fetch the revision explicitly and check out
  `FETCH_HEAD` (refs such as `refs/pull/N/head`).

## Invariants

- No secret (password, token, private key, client key, URL password) appears in any
  command's arguments; asserted for every recorded command in the HTTPS (basic, bearer,
  URL password, client certificate) and SSH tests.
- Credential files exist only during the command that needs them, with mode 0600 inside a
  0700 directory; asserted after each command and after `cleanup`.
- Error messages are redacted (`*CommandError.Message`, sandbox stderr tail).
- The child environment is exact: no process variables leak (canary test), git never reads
  `~/.gitconfig` or credential helpers, and never discovers a repository above the working
  directory.
- HTTPS credentials are offered only to the repository's own origin: askpass answers only
  prompts for that origin, and the preemptive header is scoped to
  `http.<origin>/.extraHeader` with `http.<origin>/.followRedirects=false` (a redirect to
  another origin would otherwise receive it; regression-tested with a redirecting server).
- A working copy only ever holds one repository location: `Init` discards a repository
  whose origin is another location, and `RepoKey` never maps locations a server may
  distinguish (path case, `.git` suffix) to one key.
- Revisions given to `Fetch` / `CheckoutRevision` cannot carry refspecs.
- `RepoLock`: different paths never block each other; an exclusive holder is alone; joiners
  of a shared revision never proceed before `init` finished; `init`'s closer runs once
  when the last holder releases; waiting honors the context.
- `NormalizeURL` is idempotent, strips credentials, and equivalent spellings (scp-like /
  `ssh://`, host case, default ports, `.git` / trailing slash) are `SameURL`; spellings
  that agree on path case and the `.git` suffix share `RepoKey`, and equal keys imply
  `SameURL` (property-tested).
- `MaxVersion` returns one of the candidate tags, satisfying the constraint, with no greater
  satisfying candidate (property-tested).

## Failure modes

| Situation | Result |
|-----------|--------|
| Empty/option-like/control-character URL, remote-helper transport (`ext::`, `fd::`), unsupported scheme, missing host | `ErrInvalidURL` from `New` |
| `file://` URL or local path without `AllowLocalRepositories(true)` | `ErrInvalidURL` |
| Revision/branch/namespace starting with `-`, containing whitespace/control characters, or not a valid ref name | `ErrInvalidRevision` (nothing runs) |
| Refspec-shaped revision (`a:b`, `+a`, `^a`, globs) or revision expression (`main~1`) given to `Fetch` / `CheckoutRevision` | `ErrInvalidRevision` (nothing runs) |
| Existing working copy whose `remote.origin.url` is missing or another location | discarded and re-initialized (Info log with the sanitized client URL) |
| HTTP redirect while a bearer / forced-basic header is configured | redirect refused: `*CommandError` (`The requested URL returned error: 30x`) |
| Working-copy method before `Init` / client without root | `ErrNotInitialized` |
| Remote rejects credentials, prompt needed, `Permission denied (publickey)`, host key verification failure | `*CommandError` matching `ErrAuthentication` |
| Unknown ref / object | `*RevisionNotFoundError` or `*CommandError` matching `ErrRevisionNotFound` |
| Timeout / cancellation | `*CommandError` matching `sandbox.ErrTimeout` (+ `context.DeadlineExceeded`) / `context.Canceled` |
| `ls-remote` output over 64 MiB | `sandbox.ErrOutputLimit` |
| Submodule clone failure (including `file://` submodules under git's default `protocol.file.allow=user`) | `failed to update submodules: ...` |
| `CommitAll` without changes | unchanged HEAD SHA + `ErrNothingToCommit` (messages get `--cleanup=whitespace`: `#` lines are kept, as upstream's `commit -m`) |
| Note missing | `ErrNoteNotFound`; notes push conflicts retried (50 ms → 1 s, ≤ 5 s), then `failed to push note after N attempt(s)` |
| Unsigned commit/tag | `ErrUnsigned` (`RevisionMetadata.Signature.Result = unsigned`) |
| Bad / expired / revoked signature, key missing from keyring, good signature refused by git | `*SignatureError` (`ErrSignatureInvalid`) with `Info.Result` |
| `RepoLock` init failure | `failed to initialize repository resources: ...`; next holder gets `clean=true` |

## Security notes

- Credentials never on argv (`/proc/<pid>/cmdline`) or in `.git/config`. The bearer /
  forced-basic header is in the git process environment (as upstream's
  `ARGOCD_GIT_*_AUTH_HEADER`), readable only by the same UID.
- Logging: `slog` records contain the sanitized URL, the working-copy directory base name
  and never arguments, environment, output or headers. Commit-reference trailer problems log
  at most 100 runes of the value.
- Repository hooks never run (`core.hooksPath=/dev/null`, `--no-verify`, templates
  disabled); `core.fsmonitor` is forced off; `ext::` transports are refused twice (URL
  validation and `protocol.ext.allow=never`); `safe.bareRepository=explicit` blocks
  embedded bare repositories.
- Local repositories are refused by default so a tenant-controlled URL cannot read other
  working copies or server files.
- Redirects: git's default `http.followRedirects=initial` rebases later requests onto the
  redirect target but keeps sending the `http.extraHeader` loaded for the original URL. A
  repository (or open redirect) on the credential's origin could therefore forward a bearer
  token or forced basic credentials to another host. Whenever a preemptive header is
  configured, redirects are disabled for its scope (`http.<origin>/.followRedirects=false`).
  Askpass-only credentials keep git's default (redirects followed): the redirect target
  sees no credentials and the helper refuses its prompts (both tested).
- Working copies are keyed by `RepoKey`, which keeps path case and `.git` suffixes, and
  `Init` verifies the stored origin, so two repositories can never share refs, objects or
  files through one root, and the ref cache never serves one repository's listing to
  another.
- Revisions reaching `git fetch` are restricted to object and ref names, so a
  caller-supplied revision cannot force-update refs of the shared working copy.
- SSH keys apply to every SSH host contacted by the command (submodules included), as
  upstream; HTTPS secrets are origin-scoped (stricter than upstream).
- Checked-out trees are untrusted: callers must run `sandbox.CheckSymlinks(root, ".git")`
  before rendering (SRC-117); this package does not inspect content.
- Commit messages and notes are passed through stdin, never as arguments.
- `RefCache` stores no secrets: keys hold an HMAC (random per-process key) of the
  credentials.

## Parity feature areas served

| Feature | How |
|---------|-----|
| SRC-104 git revision resolution | `ResolveRevision` (HEAD, branch, tag, peeled annotated tag, full/short SHA, `refs/pull/N/head`, exact error text) |
| SRC-105 semver constraints, SRC-106 `tagPrefix` | `MaxVersion`, `TagPrefix` option |
| SRC-107 git reference cache | `RefCache` (TTL, coalescing, `read=false` bypass); distributed lock not applicable (no Redis, ADR-0004) |
| SRC-108 checkout and fetch strategy | `CheckoutRevision`, `Fetch`, `Checkout` |
| SRC-109 / REPO-054 submodules | `Checkout(..., true)`, `SubmodulesEnabled` |
| SRC-116 repository locking | `RepoLock` |
| SRC-126 `ListRefs` | `LsRemote` (sorted short names) |
| SRC-128 `GetRevisionMetadata` | `RevisionMetadata`, `ParseReferences` |
| SRC-134 `UpdateRevisionForPaths` building block | `ChangedFiles` |
| REPO-005/006/007/008/009/010 credentials | `HTTPSCreds`, `SSHCreds`, `Insecure` |
| REPO-012 `enableLfs` | `EnableLFS` (implemented, not exercised: git-lfs absent in the test environment) |
| REPO-021/022 proxy / noProxy | `Proxy`, `NoProxy` |
| REPO-023 SSH over SOCKS5 | `SSHCreds.Proxy` (env/script asserted; no SOCKS server test) |
| REPO-024 shallow `depth` | `Depth` |
| REPO-051 askpass and auth headers | askpass helper, scoped `http.extraHeader` via `GIT_CONFIG_*` |
| REPO-052 built-in git config | `BuiltinGitConfig` |
| REPO-053 retry settings | `Retry` (attempts, base, max; factor 2) |
| REPO-057/058 known hosts / TLS certs | `SSHCreds.KnownHosts`, `HTTPSCreds.CAData` (callers load the ConfigMap data) |
| REPO-069/070 GPG `none`/`head` building block | `VerifySignature` (commit or annotated tag) |
| APP-119/121/122/128/129 hydrator writes, notes, author, trailers | `CheckoutOrOrphan`, `CheckoutOrNew`, `RemoveContents`, `CommitAll`, `Push`, `AddNote`, `GetNote`, `SetAuthor`, `ParseReferences` |

## Adapting configuration (callers)

| Source (CRD / settings) | Patrol mapping |
|-------------------------|----------------|
| Repository Secret `password`/`bearerToken` set | `HTTPSCreds{Username, Password, BearerToken, ForceBasicAuth, ClientCertData, ClientCertKey, Insecure}` (credential precedence REPO-026 is the caller's) |
| `sshPrivateKey` (no password/token) | `SSHCreds{PrivateKey, InsecureIgnoreHostKey: insecure \|\| insecureIgnoreHostKey, KnownHosts: <ssh_known_hosts data>, Proxy}` |
| no credentials | `NopCreds{}` |
| `argocd-tls-certs-cm` entry for the host | `HTTPSCreds.CAData` (or the client `Insecure` option) |
| `insecure` | `HTTPSCreds.Insecure` / `Insecure(true)` |
| `enableLfs`, `depth`, `proxy`, `noProxy` | `EnableLFS`, `Depth`, `Proxy`, `NoProxy` |
| `spec.source.tagPrefix` | `TagPrefix` |
| `ARGOCD_EXEC_TIMEOUT` / `ARGOCD_EXEC_FATAL_TIMEOUT` | `Timeout` / `KillGrace` |
| `ARGOCD_GIT_ATTEMPTS_COUNT`, `ARGOCD_GIT_RETRY_DURATION`, `ARGOCD_GIT_RETRY_MAX_DURATION` | `Retry(attempts, base, max)` |
| `ARGOCD_GIT_MODULES_ENABLED` / `reposerver.enable.git.submodule` | `SubmodulesEnabled` |
| `reposerver.enable.builtin.git.config` | `BuiltinGitConfig` |
| `--revision-cache-expiration`, `noRevisionCache` | `NewRefCache(ttl, n)`, `WithRefCache(cache, !noRevisionCache)` |
| GnuPG keyring (`ARGOCD_GNUPGHOME`) | `VerifyCommitSignature(..., gnupgHome)` / `GnuPGHome` |
| shared-memory temp dir for keys | `CredentialsDir` |
| persistent checkout directory | `filepath.Join(cacheRoot, RepoKey(url))` as `root` |

## Verification

Tests live next to the code and are hermetic: local bare repositories under `t.TempDir()`
(`file://`), a local git smart-HTTP server (`net/http/cgi` + `git http-backend` from
`git --exec-path`) behind basic auth / bearer / TLS / mTLS checks, an `httputil` forward
proxy, a throwaway `sshd` on 127.0.0.1 (skipped when sshd cannot start), and GnuPG keys
generated in temporary `GNUPGHOME`s (skipped when `gpg` is absent). Covered: branch, tag,
annotated tag, lightweight tag, semver (range, `*`, `*-0`, wildcard, exact pin, tag prefix,
no match), non-standard refs, full and short SHA, HEAD; ls-remote summary and empty repos;
ref cache coalescing, credential separation, bypass, TTL expiry and bounds; init/fetch/
checkout and clean (tracked, untracked, nested repositories); checkout strategy fetch
counts and FETCH_HEAD fallback; shallow fetch; submodules (enabled, disabled per call and
server-wide, default protocol policy); changed files including renames; metadata and
trailers; HTTPS basic via askpass (correct, wrong, missing, default username, password in
URL), bearer, forced basic, custom CA / insecure, client certificates, proxy / noProxy,
push over HTTP; SSH known host / unknown host / insecure / unauthorized key; GPG good
signature, signed tag, unsigned commit and tag, missing key, metadata signature; redirects
with bearer / forced basic (refused, target never sees `Authorization`) and with askpass
credentials (followed, target sees nothing, prompts for it refused); refspec-shaped
revisions rejected before `fetch` with no local ref changed; two distinct repositories
(`remote.git` / `remote`) sharing a root or a ref cache never see each other's content;
commit messages keep `#` lines; hydrator
flow (orphan branch, commit with signoff, nothing to commit, remove contents, force push),
`CheckoutOrNew`, notes (missing, round trip, overwrite, default namespace, concurrent
writers); `RepoLock` semantics; exact environment; timeouts and cancellation; retries;
redaction; URL, `MaxVersion`, ref-list and `GIT_CONFIG_*` round-trip properties (rapid).
Exact commands and results are reported by the implementing task.

## Known gaps and deliberate deviations

- Upstream lists refs in-process with go-git; Patrol runs `git ls-remote` (same ref
  semantics; ordering is git's sorted listing, so a branch wins over a tag with the same
  short name deterministically).
- Truncated SHAs are expanded to full SHAs (upstream returns them unchanged).
- `ChangedFiles` uses `--no-renames` and `-z`: a rename lists both paths (upstream lists
  only the new path) and paths are not C-quoted.
- Checkout always ends with `clean -ffdx` (upstream only when `cleanState` or submodules);
  `RepoLock`'s `clean` flag is still provided for callers' own state.
- With a bearer token or `ForceBasicAuth`, HTTP redirects are refused (upstream follows
  them and keeps sending the header to the new origin). Askpass-only credentials still
  follow redirects.
- `RepoKey` (working-copy and ref-cache key) is finer than upstream's `NormalizeGitURL`:
  `repo` vs `repo.git` and path-case variants get separate working copies and listings
  (more disk and fetches when an installation mixes spellings of one repository; never
  another repository's content). scp-like and `ssh://` spellings still share a key, as
  upstream; on a generic sshd where `host:path` is home-relative and `ssh://host/path` is
  absolute these could be different repositories, in which case `Init` only replaces the
  URL (residual risk, not observed on hosting services).
- `CheckoutRevision` accepts only `""`, `HEAD`, SHAs and ref names (upstream passes
  resolved SHAs); revision expressions such as `main~1` are rejected because the
  revision may be fetched.
- HTTPS credentials and headers are only offered to the repository origin; submodules on
  other hosts no longer receive them (upstream offers them to any prompt).
- `file://` and local-path repositories are refused unless `AllowLocalRepositories(true)`.
- `RevisionMetadata.Message` is the full trimmed message, as upstream code (the parity
  inventory's "trailers removed" describes `ParseReferences`' second result).
- Not implemented: GPG `strict` mode history walk with seal commits (upstream
  `LsSignatures`, REPO-071), `LsFiles` / globbing for `GetGitFiles` (SRC-132/133),
  `HasFileChanged`, GitHub App / Azure / GCP credential types (REPO-015…), the
  distributed (Redis) ref-cache lock (not applicable, ADR-0004), event handler hooks for
  metrics (`OnLsRemote`, `OnFetch`, `OnPush`).
- Git LFS paths are implemented but not exercised (no git-lfs in the test environment;
  the test is skipped). The SOCKS5 proxy path is asserted on the generated environment
  only (no `connect-proxy`/SOCKS server locally).
- Requires git ≥ 2.31 (`GIT_CONFIG_COUNT`); tested with git 2.56.0, OpenSSH 10.3p1 and
  GnuPG 2.5.24 on darwin/arm64.
- `MaybeAskpassMain` must be wired into `cmd/patrolcd` main by its owner.
