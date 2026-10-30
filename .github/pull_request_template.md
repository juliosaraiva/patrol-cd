## What and why

<!-- The change, the reason, and the issue it closes. -->

Closes #

## Checks

<!-- docs/guides/claude-code-sdlc.md. Paste the outcome lines, not the full logs. -->

- [ ] `make sdlc-check` passed on the pushed commit (CI static checks, tests of what changed, CodeQL)
- [ ] Agent review (`/review-change`) recorded with no blocking findings, or documentation only
- [ ] Tests added or changed with the behaviour; executed checks recorded in `docs/verification/log.md` where the issue asks for evidence
- [ ] Parity statuses changed only with a test that exercises the feature (`docs/parity/status/*.yaml`)

## Minor review findings not addressed

<!-- One line each, or "none". This repository is public: a finding that is a vulnerability goes to a
private report (SECURITY.md), never here. -->

## Not covered

<!-- What this change does not do or could not check, honestly. -->
