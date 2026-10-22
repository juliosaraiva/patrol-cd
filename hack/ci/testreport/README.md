# testreport

`go run ./hack/ci/testreport [-skips FILE] [-must-run FILE] [-summary FILE] [-json-out FILE] [-stderr FILE] [-title TEXT] [-strict-allowlist] [-require-packages] [input.json|-]`
reads the `go test -json` stream of a CI job (a file, or stdin) and prints a plain text report:
package and test totals, the 15 slowest packages and top-level tests, every failure with up to
60 lines of its output, and every skipped test with the message of its `t.Skip` call. `-summary`
appends the same report as Markdown (for `$GITHUB_STEP_SUMMARY`, kept under 900 KiB so that it
stays below GitHub's 1 MiB limit for one step) and `-json-out` writes it as JSON with a stable
field order and no timestamps. `-stderr FILE` adds the last 40 lines of go test's own stderr to
the reports; the tool shows that text and never parses it.

The headline (`Result:` in the text report, the first line of the Markdown, `result` in the
JSON) is the outcome of the whole run: `OK`, `FAILED:` with the failed or unfinished packages,
builds and tests followed by the skip policy outcome (`skip policy OK`, or the forbidden skips
and must-run violations), or `NO TEST EVENTS`. The JSON also has the two outcomes apart:
`tests_result` (`ok`, `failed`, `no-events`) and `policy_result` (`ok`, `violations`).

The exit status follows the headline: 0 for `OK`; 1 for `FAILED`, that is a failed or
unfinished test, package or build, a skip that matches no line of `hack/ci/allowed-skips.txt`,
or a test listed in `hack/ci/must-run.txt` that did not pass in this input (`go test`'s own exit
status reports test failures too; this one also catches a `TestMain` that drops the code of
`m.Run`, after which `go test` exits 0); 2 means a usage error, an unreadable or malformed rule
file, or an input without a single test event (the reports are still written). The allowlist
has one rule per line,
`PACKAGE_REGEXP<TAB>TEST_REGEXP<TAB>REASON_REGEXP<TAB>comment`, each regexp matched against the
whole import path, test name and skip message (a regexp that does not parse on its own, such as
one with a stray `)`, is rejected), with a mandatory comment saying why the skip is legitimate
on CI; the must-run list has `PACKAGE<TAB>TEST_REGEXP<TAB>comment` and only applies to packages
present in the input, so one list serves every job (`-require-packages` makes absence a
failure). The header comment of `main.go` has the full rules, including how parents whose
subtests all skipped are folded into one entry, how the skip message is found (split and
newline-less output, continuation lines, a message lost in a line cut at 8 KiB, a log written by
a `t.Cleanup` after `t.Skip`), and when an allowlist line counts as stale. Carriage returns in
test output and skip messages are printed as `\r` in the text and Markdown reports and in
suggested allowlist lines (the allowlist is matched against the raw message, which `-json-out`
keeps in `reasons[].text`): raw, a lone CR ends a line of the job log, so `::error` after it
would be a workflow command, and it ends a row of the Markdown tables. That keeps log lines and
table rows intact; it does not stop the runner acting on `##[error]`-style commands, which it finds
anywhere in a line. `ci_go_suite` (`hack/ci.mk`) stops workflow commands around the report with a
random `::stop-commands::` token when it runs on GitHub Actions.

`go run ./hack/ci/testreport -check-rules [packages.txt|-]` reads no test events: it checks the
rule files against the packages of the module, the list being `go list ./...` output from the
file, from stdin (`-`), or from running `go list ./...` in the current directory when no path is
given. Every package named in `-must-run` must exist and every package regexp of `-skips` must
match at least one package, so that renaming or moving a package cannot silently turn a rule
into dead text. It prints one line per problem and exits 1 when there is one (2 when a rule file
or the package list cannot be read). `make ci-static` runs it.

The fixtures under `testdata/` are of three kinds. `real-darwin.json` is a real capture from
the developer machine (macOS arm64, go1.27.3, 2026-10-21), made with
`go test -json -count=1 -run '^(TestLFS|TestVerifyCommitSignature|TestConformance)$' ./internal/git/ ./internal/health/ ./internal/oci/ocitest/ ./pkg/scm/gogs/ 2>&1`
(exit status 0): `TestLFS` skips there because `git-lfs` is not on PATH, the gogs conformance
suite skips the capabilities the provider does not declare, `internal/health` runs no test
under that `-run` and `internal/oci/ocitest` has no test files. `real-probe.json` is a real
capture (macOS arm64, go1.27.3, 2026-10-21) of the probe tests in `real-probe/a_test.go`, made
in a scratch module `example.com/probe` holding that file and a `package a` file in `a/`, and a
`b/` package without test files, with `go test -json -count=1 ./... >real-probe.json` (exit
status 1: `TestFail` fails on purpose). It covers a parent that skips after a passing subtest,
a group with allowed and forbidden reasons, three levels of subtests, parallel subtests, output
without a newline before a skip message and before `SkipNow`, a `t.Cleanup` log after `t.Skip`
(guarded and unguarded), a continuation line that looks like a `file.go:N:` header, a 3 KB
message split by test2json into 1024-byte events, a 10 KB message and a 25-line message.
`real-edge.json` and `real-mainexit0.json` are real captures (macOS arm64, go1.27.3, 2026-10-22)
of the tests in `real-edge/`, made in a scratch module `example.com/edge` holding its three
packages, with `go test -json -count=1 ./lost/ ./cr/ >real-edge.json` (exit status 1: `cr`
fails on purpose) and `go test -json -count=1 ./mainexit0/ >real-mainexit0.json` (exit status 0).
`lost` logs a message that `real-edge-skips.txt` allows, prints output without a newline (more
than 8 KiB of carriage-return progress, a lone tab, a lone carriage return, short progress), then
skips for a reason the allowlist does not have; `cr` prints carriage returns followed by
`::error` and `::warning` in a failing test and in a skip message; `mainexit0` has a `TestMain`
that exits 0 after `m.Run`, so its package passes although `TestFails` failed.
`synthetic.json` is written by hand in the exact shape go 1.27 emits (checked against probe runs
of a scratch module): a failure with `t.Error` output, skips with one-line and multi-line
reasons, a parent whose subtests all skipped, a non-JSON `go: downloading` line, a build
failure, a package without test files, one that ran no test and one killed by `-timeout`. The
`*-skips.txt` and `*-must-run.txt` files are the rule files the tests use with them.
