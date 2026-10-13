# ADR-0013: Validation strategy

Status: accepted — 2026-10-12

## Decision
- Structural OpenAPI schemas, enums, defaults and CEL `x-kubernetes-validations` in every CRD
  reject malformed objects at admission without a webhook (e.g. exactly one of
  `source`/`sources`/`sourceHydrator`, destination `server` xor `name`, `Key=Value` format and
  count/length bounds for sync options — option values are validated by the controller —,
  matrix/merge child counts, Operation spec immutability except `cancel`).
- A ValidatingAdmissionPolicy (`deploy/crds/operations-admission-policy.yaml`) prevents users
  from forging `Operation.spec.initiatedBy` (other usernames or `automated: true`) unless they
  hold the `impersonate` verb on `operations.cd.patrol.example` (granted only to Patrol
  components).
- Semantic validation that needs other objects (project permissions, repository existence,
  cluster existence) runs in the API server (rejects with 4xx) and in the controller
  (surfaces `InvalidSpecError` conditions), mirroring upstream behavior and covering objects
  created directly with kubectl.
- An optional validating webhook (`patrol-server --enable-admission-webhook`) can enforce the
  semantic checks at admission; disabled by default to avoid an availability dependency.
