# ADR-0009: Authentication and authorization

Status: accepted — 2026-10-12

## Decision
- Sessions: Patrol-issued JWT (HS256, key `server.secretkey` in `patrol-secret`, rotated by
  key ID) for local logins and after OIDC login. Claims: `sub`, `groups`, `email`, `jti`,
  `iat`, `exp`, `iss=patrol`. Browser: `patrol.token` cookie, `HttpOnly`, `Secure`,
  `SameSite=Strict`; state-changing requests additionally require
  `X-Requested-With: patrol` (CSRF defense in depth).
- Logout revokes the `jti` (stored until expiry in `patrol-session-revocations`).
- API tokens: account tokens and project-role tokens (JWT with `jti`), metadata stored in
  `patrol-secret` / `AppProject.status.jwtTokensByRole` as upstream; deleting the metadata
  revokes the token.
- OIDC: authorization code + PKCE via go-oidc; Dex optional. Group claims mapped through the
  configured claim names; `scopes` in `patrol-rbac-cm` select which claims are subjects.
- RBAC: Patrol's own evaluator for the Casbin-style grammar used by Argo CD
  (`p, sub, res, act, obj, eft` / `g, sub, role`), glob or regex match mode, explicit deny
  wins, `policy.default`, built-in `role:readonly` and `role:admin`. Enforced in the API
  server for every request including streams, terminal and extension proxy calls.
- Audit: every mutating API call and every authorization denial emits a structured audit log
  line (actor, action, resource, result, revision, operation) and, for Application/Project
  targets, a Kubernetes Event.
