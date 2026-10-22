# Releasing

Releases are cut from `main` without hand-editing versions or changelogs. The commit subjects
(conventional commits, as the repository already uses) decide the next version and become
`CHANGELOG.md`; a release pull request carries the change; merging it is the decision to release,
and the release workflow publishes once CI has passed on the merged commit.

> **Status.** The tool (`hack/release`) is unit-tested and was run against this repository's
> history and in a scratch clone (prepare, generated-file checks, artifacts); see
> [`docs/verification/log.md`](../verification/log.md) V-0037. The two workflows have **not run on
> GitHub yet**: the first release pull request, image push, chart push and GitHub release are
> unobserved.

## The flow

```
push to main ──► Release PR workflow ──► release/next branch + pull request "chore(release): vX.Y.Z"
                 (release-pr.yml)          CHANGELOG.md section, Chart.yaml version/appVersion,
                                           chart README and static manifests regenerated
merge the PR ──► CI on main (full tier) ──► Release workflow (release.yml), from the tested commit:
                                              artifacts │ image ──► chart push, tag vX.Y.Z, GitHub release
```

1. **Every push to `main`** runs `release-pr.yml`. `hack/release` reads the commits since the last
   `vX.Y.Z` tag. If one of them is releasable it rebuilds the branch `release/next` from `main` with
   one commit, `chore(release): vX.Y.Z`, and opens or updates the pull request. Later pushes to
   `main` rebuild it, so it always describes `main` as it is.
2. **Review and merge the pull request.** Reword `CHANGELOG.md` lines in it if needed, right
   before merging (the next push to `main` would rebuild the branch and drop the edit).
3. **CI runs on the merge** (it changes the chart, so the full tier). When the CI workflow succeeds
   on a push to `main`, `release.yml` checks whether a release is *pending*: `Chart.yaml` names
   X.Y.Z, `CHANGELOG.md` has its section and the tag `vX.Y.Z` does not exist. If so it publishes:

   | Job | What | Where |
   |-----|------|-------|
   | `artifacts` | `make release-artifacts`: `patrol` CLI for linux, darwin, windows × amd64, arm64; `install.yaml`, `namespace-install.yaml`, `core-install.yaml`, `ha-install.yaml`, `ha-namespace-install.yaml`, the `*-with-hydrator.yaml` variants and `crds.yaml`, all pointing at the published image; `patrol-cd-X.Y.Z.tgz`; `SHA256SUMS` | workflow artifact |
   | `image` | `make image-multiarch PUSH=1`: linux/amd64 + linux/arm64, SBOM and provenance attestations, stamped `vX.Y.Z` | `REGISTRY/patrol-cd/patrol-cd:X.Y.Z` |
   | `publish` | checks `SHA256SUMS`, `helm push`, then `gh release create vX.Y.Z --target <tested sha>` with the changelog section, install commands, image digest and the artifacts | `oci://REGISTRY/charts/patrol-cd`, GitHub release |

   The tag is created last: it exists only when everything else was published. A failed run can be
   re-run from the Actions page or with **Run workflow** on `release.yml` (pushing the same image
   tag and chart version again replaces them).

Nothing new is proposed while a release is pending, so the release pull request does not reappear
between the merge and the tag.

## Versions

One number for everything: the git tag `vX.Y.Z`, the chart's `version` and `appVersion`, the image
tag `X.Y.Z` (the chart's default `image.tag` is its appVersion) and the version stamped into both
binaries (`vX.Y.Z`).

| Commits since the last tag | Next version |
|----------------------------|--------------|
| a breaking change: `type!:` or a `BREAKING CHANGE:` trailer | major; **minor before 1.0.0** |
| `feat` | minor |
| `fix`, `perf`, `revert` | patch |
| only `docs`, `test`, `chore`, `ci`, `build`, `refactor`, `style` | no release |

The first release (no tag yet) is the chart's current version, `0.1.0`. To choose a version
yourself (1.0.0, say), run the **Release PR** workflow manually with `version` set; it must be
newer than the last tag. Pre-releases (`-rc.1`) are not produced.

## Changelog

`CHANGELOG.md` is generated, newest release first. Each release section lists *Breaking changes*
(with the trailer's text), *Features*, *Bug fixes*, *Performance* and *Reverts*; other types are
left out. Each line is `**scope:** description`, the pull request it arrived with (from the
`Merge pull request #N` merge commit, or a squash merge's `(#N)` suffix) and the commit. Merge
commits themselves are skipped. Commit subjects are untrusted input: `<`, `>` and `&` are escaped
and control characters removed before they reach the changelog, release notes or pull request body.

## Commit subjects are checked

The `static` CI job runs `make release-lint-commits` on pull requests: every non-merge commit
between the base and the head must be `<type>(<scope>): <description>` (scope optional, `!` for a
breaking change) with a type from the table above. Reword offending commits
(`git rebase -i`, then force-push) or squash-merge with a conventional title.

## Repository settings

| Setting | Why |
|---------|-----|
| Settings → Actions → General → *Allow GitHub Actions to create and approve pull requests* | `release-pr.yml` opens the pull request with the workflow token. Required. |
| Secret `RELEASE_TOKEN` (optional) | Pull requests opened with `GITHUB_TOKEN` do not start CI. A fine-grained token or GitHub App token with *contents* and *pull requests* write on this repository makes CI run on the release pull request. Without it, close and reopen the pull request (or push to it) to start CI. |
| Variable `RELEASE_REGISTRY` (optional) | Where the image and chart go, `host[/namespace]`. Default `ghcr.io/<owner>`. |
| Secret `RELEASE_REGISTRY_TOKEN` (optional) | Registry password when `RELEASE_REGISTRY` is not GHCR; for GHCR the workflow token (`packages: write`) is used. |

Packages pushed to GHCR from a private repository are private: grant pull access (or make the
package public) before installing from them, or give the cluster an image pull secret.

## Locally

```bash
make release-next                      # the version the next release pull request would propose
make release-preview                   # its CHANGELOG.md section
make release-lint-commits              # check origin/main..HEAD
make release-prepare                   # what the workflow commits (needs helm 4 and make tools)
make release-artifacts RELEASE_VERSION=0.1.0 RELEASE_REGISTRY=ghcr.io/juliosaraiva   # dist/release
```

`go run ./hack/release status` prints the release state the release workflow acts on
(`current`, `tag`, `tagged`, `changelog`, `pending`). Nothing here pushes, tags or publishes.

## Not covered yet

Signing (cosign) of the image, chart and checksums; GitHub artifact attestations for the binaries;
pre-releases and release branches for patch releases of older minors; a Homebrew tap or other
package managers.
