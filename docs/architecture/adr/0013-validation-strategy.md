# ADR-0013: Validation strategy

Status: accepted — 2026-10-12

## Decision
- Structural OpenAPI schemas, enums, defaults and CEL `x-kubernetes-validations` in every CRD
  reject malformed objects at admission without a webhook (e.g. `source` xor `sources`,
  destination `server` xor `name`, sync option enums, cron syntax shape).
- Semantic validation that needs other objects (project permissions, repository existence,
  cluster existence) runs in the API server (rejects with 4xx) and in the controller
  (surfaces `InvalidSpecError` conditions), mirroring upstream behavior and covering objects
  created directly with kubectl.
- An optional validating webhook (`patrol-server --enable-admission-webhook`) can enforce the
  semantic checks at admission; disabled by default to avoid an availability dependency.
