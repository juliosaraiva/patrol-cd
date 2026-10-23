# Security policy

## Reporting a vulnerability

Please report security vulnerabilities privately, through GitHub's private vulnerability reporting:
the **Security** tab of this repository, **Report a vulnerability**. Do not open a public issue or pull
request, and do not mention the problem in commit messages or discussions until a fix is released.

Include what you can of: the affected component (controller, API server, repo-server, CLI, UI, chart),
the version or commit, a description of the impact, and steps or a proof of concept to reproduce it.

Reports are handled on a best-effort basis by the maintainer: an acknowledgement, an assessment, a fix
developed in a private fork through a draft security advisory, then a release and a published advisory
that credits the reporter unless they prefer otherwise.

## Supported versions

Patrol CD has no release yet. Until the first release, only the `main` branch is maintained, and fixes
land there. Once releases exist, this section will list the versions that receive security fixes.

## Scope

In scope: the code in this repository, the container image built from its `Dockerfile`, the Helm chart
and the static manifests under `deploy/`, and the GitHub workflows. The deliberately committed
test-only SSH host key in `test/e2e/fixtures/git-server/` is not a secret (it is marked as such and only
used by the disposable e2e git server).

Out of scope: vulnerabilities in third-party dependencies that do not affect Patrol CD as it uses them
(report those upstream), and findings that need an already compromised cluster administrator.
