# Component: `internal/sandbox`

Status: implemented (phase 1). Decision record: [ADR-0007](../adr/0007-rendering-isolation.md).

## Responsibilities

1. Run every external tool used by repo-server, cmp-server and the jsonnet child
   (`git`, `helm`, `kustomize`, `patrolcd jsonnet-eval`, CMP commands) with:
   an exact environment (nothing inherited), its own process group, a wall-clock
   timeout, optional SIGTERM grace, output caps, CPU/address-space/open-file rlimits,
   and only stdin/stdout/stderr as open descriptors (in both direct and helper mode).
2. Keep file-system access to untrusted trees inside a root: secure path joins,
   out-of-bounds symlink scans, safe tar/tar.gz extraction with limits, and a
   deterministic tar.gz writer for streaming source trees to CMP sidecars.

The package does not import `api/v1alpha1`. Callers translate settings into the
neutral `Limits` / `ArchiveLimits` / exclude lists (see "Adapting configuration").

## Public API

| Symbol | Purpose |
|--------|---------|
| `Run(ctx, Cmd) (Result, error)` | Execute one command. Unix only (`errors.ErrUnsupported` elsewhere). Marks every descriptor of the calling process close-on-exec before a direct-mode start. |
| `Cmd{Path, Args, Dir, Env, Stdin, Limits, Redact}` | `Path` absolute or a bare name looked up in **`Env`'s** `PATH` (relative PATH entries ignored); `Args` exclude argv[0]; `Env` is exact (nil = empty). |
| `Limits{Timeout, KillGrace, MaxStdout, MaxStderr, CPUSeconds, AddressSpaceBytes, MaxOpenFiles}` | Zero = default (`DefaultTimeout` 90s, `DefaultMaxStdout` 256 MiB, `DefaultMaxStderr` 16 MiB, rlimits unset); negative timeout/caps = disabled. |
| `Result{Stdout, Stderr, Duration}` | Captured output (truncated at caps, **not** redacted). |
| `ErrTimeout`, `ErrOutputLimit`, `ErrInvalidCommand`, `*ExitError{Code, Signal, StderrTail}` | Run errors. Context deadline matches both `ErrTimeout` and `context.DeadlineExceeded`; cancellation matches `context.Canceled`. |
| `MaybeHelperMain()` | Must be the first call in `main` of `cmd/patrolcd` (and in `TestMain` of packages whose tests set rlimits). Enters rlimit-helper mode when `os.Args[1] == HelperArg` (`__patrol-sandbox-exec`); otherwise records that the helper is installed. |
| `SetHelperPath(path)` | Override the helper binary (default `os.Executable()`). The override must also call `MaybeHelperMain`; the handshake enforces it. |
| `BaseEnv(extra...)` | `PATH`, `HOME`, `TMPDIR`, `LANG=C.UTF-8` + extras (later keys replace earlier ones in place). |
| `SecureJoin(root, rel)` | Kernel-equivalent symlink resolution confined to root, with bounded work. |
| `CheckSymlinks(root, skip...)` / `CheckSymlinksContext(ctx, ...)` | Upstream `CheckOutOfBoundsSymlinks` semantics + physical chain check + work budgets; `*OutOfBoundsSymlinkError{Path, Target}`. A symlinked root is reported as `Path: "."`. |
| `ExtractTar` / `ExtractTarGz(r, dest, ArchiveLimits{MaxFiles, MaxTotalBytes, MaxFileBytes, StripExecBits})` and `...Context(ctx, ...)` | Safe extraction. Defaults: 500 000 entries (implicit parent directories included), 1 GiB total, 1 GiB per file. `dest` must not be a symlink. |
| `Tar(dir, w, excludes)` / `TarCount(...) (files, error)` / `TarCountContext(ctx, ...)` | Deterministic tar.gz of a tree for CMP streaming. `dir` must not be a symlink. |
| `ErrPathEscape`, `ErrAbsolutePath`, `ErrTooManySymlinks`, `ErrSymlinkBudget`, `ErrUnsafeArchiveEntry`, `ErrArchiveLimit`, `ErrInvalidArchive` | File-system errors (`*OutOfBoundsSymlinkError` unwraps to `ErrPathEscape`; budget failures in scans match both `ErrSymlinkBudget` and `*OutOfBoundsSymlinkError`). |

## How `Run` works

1. Resolve the executable (absolute path, or bare name in the command's own PATH).
2. If any rlimit is set, the argv becomes
   `<helper> __patrol-sandbox-exec cpu=N as=N nofile=N -- <abs target> <argv0> <args...>`.
   With the default helper (this binary) `Run` first refuses (`ErrInvalidCommand`,
   nothing started) unless this process called `MaybeHelperMain`. The write end of a
   status pipe is passed as fd 3. The helper lowers the limits (clamped to the
   current hard limit; CPU hard limit = soft + 1 s so SIGXCPU precedes SIGKILL), marks
   every descriptor above 2 close-on-exec (fd 3 included), writes the status byte
   `R` to fd 3 and `execve`s the target in place, keeping PID and process group.
   Helper setup failures write `F`, then exit 126 (setup) / 127 (target missing)
   with a stderr line. If the pipe reaches EOF without a status byte (the program
   ignored the protocol and exited, or overflowed its output), or no byte arrives
   within 30 s, the group is killed and `Run` fails with `ErrInvalidCommand`; the
   other program's output is never reported as the target's.
3. In direct mode `Run` marks every descriptor of the calling process above 2
   close-on-exec (listing `/proc/self/fd` or `/dev/fd`; this only sets a flag, the
   descriptors stay usable in-process, and `os/exec` clears it on `ExtraFiles`
   copies). The child starts with `Setpgid`, the exact env, `/dev/null` stdin when
   none is given, and two `os.Pipe`s for stdout/stderr (parent copies of the write
   ends are closed immediately so EOF is observable).
4. Reader goroutines capture up to the caps and keep draining after overflow; the
   first overflow kills the group immediately.
5. On timeout or context end: SIGTERM to the group and leader, wait `KillGrace`, then
   SIGKILL (immediately SIGKILL when `KillGrace` is 0 — upstream's default).
6. After the leader is reaped, SIGKILL goes to the whole group so background
   grandchildren never outlive the call; pipes are then drained, bounded by a 2 s
   deadline for writers that escaped the group with `setsid`.
7. Error precedence: helper not acknowledged > output limit > timeout > context >
   exit status (a timeout or cancellation before a slow helper acknowledged stays a
   timeout/cancellation). Exit errors carry
   the last 4 KiB of stderr after redacting **all** of stderr (so a secret straddling
   the cut cannot leak), cut on a rune boundary.

## Invariants

- The child environment is exactly `Cmd.Env`; a nil `Env` means an empty one (never
  the server's). Tested with a canary variable, in direct and helper mode.
- Every process in the child's process group is dead when `Run` returns.
- `Run` returns within `Timeout + KillGrace` plus a bounded drain (2 s, at most 4 s if
  a pipe cannot take a read deadline, plus 1 s for a stuck stdin copier) even if
  descendants keep the output pipes open.
- `len(Result.Stdout) <= MaxStdout`, `len(Result.Stderr) <= MaxStderr` (when capped).
- No descriptor above 2 reaches the target in either mode, including descriptors
  the server inherited or opened without `O_CLOEXEC` (tested with raw `open(2)`
  descriptors in both modes). Residual race: a raw non-`O_CLOEXEC` descriptor
  created by another thread between the sweep and the fork (cgo/raw syscalls only).
- With rlimits set, `Run` never returns success unless the helper acknowledged, so
  the output is the target's (tested with `/bin/echo` and a long-running script as
  helper, and with `MaybeHelperMain` not called).
- `SecureJoin` results are always `root` or below it, and their OS resolution (deepest
  existing prefix) stays inside the real root (fuzzed).
- If `CheckSymlinks(root) == nil`, no path made only of names (no `..`) resolves
  outside root (property-tested against `filepath.EvalSymlinks`, and mutation-checked:
  removing the physical chain check makes the property fail). A symlinked root is
  never accepted (it would not be descended).
- Resolution work is bounded: per resolution at most 255 symlink hops (the
  `filepath.EvalSymlinks` limit, above Linux's 40 and macOS's 32, so loops for those
  resolvers are loops here) and 4096 processed components; per `CheckSymlinks`/`Tar`
  walk at most 2^20 + 256 × entries components. lstat results are cached per walk
  (8 MiB cap), and components below a missing one are joined without system calls
  (one final lstat of the result still surfaces `ENAMETOOLONG`/`EINVAL`).
  A self-referencing 1 KiB target costs 2 lstat calls (the review estimated ~51k before;
  measured here: `SecureJoin` 676 ms and `CheckSymlinks` 487 ms for one such link under
  load average ~170).
- Extraction writes only through `os.Root`; nothing is created outside `dest`
  (asserted for every rejection case and fuzzed); an accepted archive passes
  `CheckSymlinks`; the tree written is the tree checked (`dest` must be a real
  directory, verified with `SameFile` after opening).
- Per-entry extraction work is O(depth): names are capped (4096 bytes, 256
  components, 255-byte components, 4096-byte link targets) before any filesystem
  work, and parents are verified by descending one component at a time with
  `os.Root.OpenRoot` (last verified parent cached). A test asserts the traversal
  count stays under 8 × depth per entry at depth 250; the old per-prefix walk
  re-introduced as a mutation, walked 665 186 components (limit 84 000) and took 52 s.
- `Tar` output is byte-identical for identical trees regardless of mtimes/ownership,
  and `Tar` → `ExtractTarGz` round-trips trees (property-tested).

## Failure modes

| Situation | Result |
|-----------|--------|
| Bad `Path` (empty, relative with separator, directory) | `ErrInvalidCommand`; nothing started |
| Bare name not in `Env` PATH / missing file / not executable | `fs.ErrNotExist` / `fs.ErrPermission` wrapped |
| Wall-clock timeout | `ErrTimeout`, group killed, partial output in `Result` |
| Context cancelled / deadline | `context.Canceled` / `ErrTimeout`+`DeadlineExceeded` |
| Output over cap | `ErrOutputLimit`, group SIGKILLed, output truncated at cap |
| Non-zero exit / signal | `*ExitError` (Code −1 and `Signal` set when signalled; `SIGXCPU`/`SIGKILL` for CPU rlimit) |
| Helper missing (bad `SetHelperPath`) | start error wrapping `fs.ErrNotExist` |
| Archive: absolute name / `..` component / backslash traversal | `ErrAbsolutePath` / `ErrUnsafeArchiveEntry` |
| Archive: name > 4096 bytes, > 256 components, a component > 255 bytes, link target > 4096 bytes | `ErrUnsafeArchiveEntry`, before any filesystem work |
| `dest` (extraction) or `dir` (`Tar`) or the `CheckSymlinks` root is a symlink | `*OutOfBoundsSymlinkError{Path: "."}` (`ErrPathEscape`); nothing extracted/streamed |
| Symlink resolution over budget (per link or per walk) | `ErrSymlinkBudget` + `*OutOfBoundsSymlinkError` from scans; `ErrSymlinkBudget` from `SecureJoin` |
| Rlimit helper did not acknowledge / `MaybeHelperMain` not called | `ErrInvalidCommand` |
| Context cancelled during a scan, extraction or tar | `context.Canceled` / `context.DeadlineExceeded` (checked per entry) |
| Archive: symlink absolute or leaving dest at any step, or chain escape found after extraction | `*OutOfBoundsSymlinkError` (`ErrPathEscape`) |
| Archive: entry under a symlinked parent, type conflicts with an existing entry, device/fifo/unsupported type, hard link to a non-regular or missing file | `ErrUnsafeArchiveEntry` |
| Archive: too many entries (implicit parent directories included), file or total too big, stream larger than its entries justify (chained/oversized PAX headers) | `ErrArchiveLimit` |
| Corrupt gzip/tar | `ErrInvalidArchive` |

On extraction errors `dest` may contain partial content; callers extract into a fresh
per-request directory and remove it on failure.

## Security notes

- Never logs arguments, environment or output. `Run` logs one `slog` record per call
  (Debug; Warn on timeout/overflow) with the command base name, duration, byte counts,
  exit code/signal. Error strings contain the base name, never arguments.
- `Cmd.Redact` scrubs error messages only (`******`, longest secret first). `Result`
  buffers are verbatim; callers must not log them unredacted.
- Bare command names are resolved only through the command's own PATH and only via
  absolute PATH entries, so a working directory full of repository content can never
  supply a binary.
- Group kill uses `kill(-pgid)`. After the leader is reaped the group ID cannot be
  reused while any member lives; with no members the kill fails with `ESRCH` (reuse
  of the ID by a new group leader in that instant would need PID wrap-around within
  microseconds). The leader itself is signalled through `os.Process`, which refuses
  processes it has reaped.
- RLIMIT_AS is enforced on Linux only; macOS accepts and ignores it (best effort, as
  ADR-0007 allows). Memory isolation in-cluster comes from container limits.
- The tar budget reader bounds decompressed bytes to Σ(file sizes) + 64 KiB per entry
  (carry-over capped at 2 MiB), so gzip bombs and chained PAX headers cannot burn CPU;
  per-file size checks reject a bomb from its header without inflating it.

## Parity feature areas served (`docs/parity/areas/02-sources-rendering.md`)

| Feature | How |
|---------|-----|
| SRC-115 exec timeout (`ARGOCD_EXEC_TIMEOUT`, SIGKILL default, SIGTERM+fatal timeout for git) | `Limits.Timeout` (default 90 s), `Limits.KillGrace` |
| SRC-088 CMP timeouts (process group, SIGTERM then SIGKILL after 5 s on ctx expiry) | `Run` with ctx deadline, `KillGrace: 5*time.Second` |
| SRC-117 out-of-bounds symlink protection (absolute or stepwise escape, dangling allowed, `.git` skipped) | `CheckSymlinks(root, ".git")` |
| SRC-046 / SRC-100 Helm chart and OCI layer extraction size limits (default 1G, disable flag) | `ExtractTarGz` / `ExtractTar` with `MaxTotalBytes` (negative = disabled) |
| SRC-085 / SRC-086 CMP tarball streaming and `--plugin-tar-exclude` (`filepath.Match` on the relative path, excluded dirs pruned, `no files to send`) | `TarCount`; error text `error verifying exclusion pattern "<p>"` matches upstream |
| SRC-083 CMP `preserveFileMode` | `ArchiveLimits.StripExecBits = !preserveFileMode` |
| SRC-012 / SRC-018 / SRC-022 path confinement for manifests, jsonnet imports, value files | `SecureJoin` building block (callers add upstream's "root itself not allowed" rule where needed) |
| SRC-089 cmp-server isolation | building blocks only (`BaseEnv`, `Run`, fresh extraction dirs) |

## Adapting configuration (callers)

| Upstream setting | Patrol mapping |
|------------------|----------------|
| `ARGOCD_EXEC_TIMEOUT` / `ARGOCD_EXEC_FATAL_TIMEOUT` | `Limits.Timeout` / `Limits.KillGrace` (git uses the grace) |
| `reposerver.helm.manifest.max.extracted.size`, `reposerver.oci.manifest.max.extracted.size` | `ArchiveLimits.MaxTotalBytes` (and `MaxFileBytes`); `disable.*` → `-1` |
| `reposerver.plugin.tar.exclusions` | `Tar` excludes |
| `reposerver.allow.oob.symlinks` | skip `CheckSymlinks` (extraction still refuses escaping links) |
| `reposerver.max.combined.directory.manifests.size` | caller-side; `MaxStdout` bounds tool output |
| CMP `preserveFileMode` | `StripExecBits` |

## Verification

Review follow-up (2026-10-13) regression tests: `TestExtractTarRejectsOverlongNamesBeforeTouchingDisk`,
`TestExtractTarDeepEntriesLinearWork`, `TestExtractTarImplicitDirectoriesCountAsEntries`,
`TestExtractTarIntoSymlinkedDestFailsClosed`, `TestTarSymlinkedRootFailsClosed`,
`TestArchiveContextCanceled`, `TestCheckSymlinksSymlinkedRootFailsClosed`,
`TestSecureJoinSelfLoopBoundedWork`, `TestCheckSymlinksSelfLoopsFailClosedCheaply`,
`TestResolveStepBudget`, `TestResolveSimpleLoopsStillAccepted`, `TestCheckSymlinksWalkBudget`,
`TestCheckSymlinksContextCanceled`, `TestRunHelperHandshake`,
`TestRunDirectModeDoesNotLeakRawDescriptors`, `TestHelperMainFailsClosed`. Each was
mutation-checked: re-introducing the original defect (per-prefix `Root.Lstat`, no
cache/budget, no symlinked-root check, ignored handshake, no direct-mode sweep) makes
it fail. Fuzzing found two regressions of the "skip lstat below a missing component"
optimization (NUL byte, over-long path accepted); both are fixed and kept as corpus
entries in `testdata/fuzz/FuzzSecureJoin`.

Tests live next to the code (`*_test.go`); `TestMain` calls `MaybeHelperMain` and
provides a `setsid` child for the escaped-writer case. Covered: grandchild kill on
timeout and cancel, background children killed after leader exit, escaped writer does
not deadlock, SIGTERM grace and escalation, output caps (exact boundary, stderr,
unlimited), canary env not inherited (direct and helper), fd non-inheritance (direct
and helper), stderr tail size/redaction/rune boundary, rlimits via helper (`ulimit`
readback for NOFILE/CPU, CPU busy loop killed by SIGXCPU/SIGKILL), helper argv
round-trip (rapid), SecureJoin table + fuzz, CheckSymlinks upstream fixtures + chains +
soundness property, tar-slip/zip-slip/link/device/limit/bomb cases, extraction fuzz,
Tar determinism and round-trip property. Exact commands and results are reported by
the implementing task; Linux-only paths (`RLIMIT_AS`, `/proc/self/fd`) were
cross-compiled and vetted but executed only on darwin so far.

## Known gaps and deliberate deviations

- Processes that leave the process group (`setsid`, daemonizing tools) are not killed;
  only the pipe drain is bounded. A Linux follow-up could use a cgroup per call or
  `PR_SET_CHILD_SUBREAPER`.
- No seccomp, namespaces or cgroups; isolation beyond rlimits is the container's job.
- The direct-mode descriptor sweep costs one directory listing plus one `fcntl` per
  open descriptor per `Run`; a raw non-`O_CLOEXEC` descriptor opened by another
  thread between the sweep and the fork can still leak (Go never opens such
  descriptors; only cgo or raw syscalls could).
- The helper acknowledgment timeout is fixed at 30 s; a helper that has not started
  by then on an overloaded node fails with `ErrInvalidCommand` (a command timeout
  shorter than that still reports `ErrTimeout`).
- Loops with long targets can exceed the 4096-component budget before the hop limit
  and are then rejected (fail closed), where upstream's lexical check accepts them;
  short loops (`a -> b -> a`) are still accepted.
- `lowerRlimit` does not compile for FreeBSD (`syscall.Rlimit` fields are `int64`
  there; pre-existing). Linux and darwin are the supported targets.
- A blocking non-`*os.File` `Stdin` reader can leave its copier goroutine running after
  `Run` returns (bounded wait of 1 s, then `Run` proceeds).
- `SecureJoin` rejects absolute symlinks even when they point inside root; upstream
  value-file resolution (SRC-022) follows them when OOB scanning is disabled.
- Extraction is stricter than upstream: `..` components are rejected even when they
  stay inside, device/fifo/unknown entries fail instead of being skipped, conflicting
  duplicates fail, and hard links are materialized (upstream silently drops them).
- Upstream `preserveFileMode: true` keeps exact modes; Patrol masks to 0755/0644.
- `Tar` fails on out-of-bounds symlinks even when the caller allows them (upstream
  sends them, and its CMP extraction then fails).
- The `Context` variants check the context between entries; a `Read`/`Write` blocked
  inside one entry is only interrupted by closing the reader/writer.
- Upstream memoizes OOB scans for 12 h per revision; caching is the repo-server's job.
- Walk errors from `CheckSymlinks`/`Tar` may contain absolute temp paths; callers
  sanitize per SRC-118.
