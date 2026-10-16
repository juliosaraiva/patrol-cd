# Fake GitHub API (placeholder)

Not implemented yet. The e2e environment will need an in-cluster GitHub REST API double for
the SCM provider, pull request and GitHub App scenarios (ApplicationSet SCM provider and pull
request generators, commit status reporting, GitHub App repository credentials).

Plan:

- Reuse the existing hermetic double `pkg/scm/github/internal/fakegithub` (realistic JSON
  shapes, Link-header pagination, rate-limit headers, the `/api/v3` Enterprise prefix, GitHub
  App JWT verification and installation tokens, request recording without credentials). It is
  an `internal` package of `pkg/scm/github`, so the wrapper has to live under that tree (for
  example `pkg/scm/github/internal/fakegithub/cmd/fakegithub`) or the double has to be moved to
  a test-support location importable from `test/e2e`.
- Wrap it in a small `main` that serves it on a port, loads seed data (repositories, branches,
  pull requests, App installations) from a file, and exposes the recorded requests for test
  assertions.
- Build it into a `patrol-cd/e2e-fake-github:dev` image (distroless or alpine base), deploy it
  as a Deployment + Service `fake-github.patrol-e2e.svc.cluster.local` next to the git server,
  and point `SCMProvider.spec.apiURL` at it. Repository URLs in its seed data should be the git
  server's (`http://git-server.patrol-e2e.svc.cluster.local/git/<repo>.git`) so generated
  Applications are syncable.
- Webhooks: `test/e2e/fixtures/webhook/send-github-push.sh` already produces signed GitHub push
  deliveries; pull request deliveries can be added to it.

Go code is outside the scope of the kind/fixtures workstream, so this directory only holds the
plan. Until the wrapper exists, GitHub behavior is covered by the provider's unit and
conformance tests (`pkg/scm/github`), which run the double in process (`simulated`).
