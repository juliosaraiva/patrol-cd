# Component: `internal/rbac`

Status: implemented (phase 1, library only). Owner task: T1.2. Decision record: ADR-0009.

Patrol's own evaluator for the Casbin-style RBAC grammar used by Argo CD v3.5.5. There is no
Casbin dependency. Matching uses `github.com/gobwas/glob` (glob mode) and Go `regexp` (regex
mode). The package has no Kubernetes or CRD dependency, and it does not import
`api/v1alpha1`.

## Responsibilities

- Parse and validate policy CSV (`p`/`g` lines) with the upstream line grammar, and report
  every problem with its source and line number.
- Compile policies once into immutable, indexed structures (patterns precompiled and
  deduplicated, rules indexed by subject and by literal resource).
- Evaluate requests the way the Argo CD API server does: default role first, the request's
  project policies, project-role token confinement, the user, then groups.
- Hold per-project role policies (AppProject `spec.roles`), validated with the upstream
  AppProject rules.
- Provide the engines for `patrol admin settings rbac validate` (`Validate`) and
  `patrol admin settings rbac can` (`Enforcer.Can`).
- Provide helpers for ConfigMap composition (`policy.csv` plus overlays), `scopes` parsing,
  claim-to-subject mapping, application RBAC object names and fine-grained sub-resource
  actions.

Out of scope: watching `patrol-rbac-cm` and AppProjects (the API server wires informers to
this package), token verification and revocation (`internal/auth`), audit records
(`internal/audit`), and HTTP error mapping (`internal/apiserver`).

## Public API

| Symbol | Purpose |
|--------|---------|
| `Parse(csv, ParseOptions) (*Policy, []ValidationError, error)` | Parses one document. `ParseOptions{MatchMode, Source, MaxBytes, MaxLineBytes}`. On any error-severity problem the policy is nil and `err` is a `*PolicyError` (`errors.Is(err, ErrInvalidPolicy)`). |
| `ParseSources([]Source, ParseOptions)` | Same as `Parse`, but for several named documents (for example `policy.csv` and `policy.x.csv`). Each problem keeps the line number from its own source. |
| `Policy` | Parsed, immutable and concurrency-safe. Methods: `MatchMode`, `NumRules`, `NumLinks`, `Roles`, `EnforceSubject` (one raw subject, no default role). |
| `ValidationError{Source, Line, Text, Severity, Message}` | `SeverityError` means the input is rejected; `SeverityWarning` means it loads but is probably wrong. |
| `NewEnforcer(...Option) *Enforcer` | Options: `WithLogger`, `WithProjectPolicies(project, []Role)`. Starts with no policy, so it denies everything. |
| `(*Enforcer).SetPolicy(builtin, user, defaultRole, matchMode) error` | Atomic replacement. `SetPolicySources` takes named user sources. `ApplyConfigMap(builtin, data)` reads `policy.csv`, the overlays, `policy.default` and `policy.matchMode`. |
| `(*Enforcer).Enforce(Subjects, resource, action, object) Decision` | `Subjects{User, Groups, ProjectRole *ProjectRole}` and `Decision{Allowed, MatchedPolicy, Reason}`. `EnforceErr` returns a `*PermissionDeniedError` (`errors.Is(err, ErrPermissionDenied)`). |
| `(*Enforcer).SetProjectPolicies(project, []Role) error`, `DeleteProjectPolicies`, `ProjectProblems` | Per-project role registry. `Role{Name, Policies, Groups}`. |
| `(*Enforcer).EnforceAppResource(s, verb, appObject, AppResource, inheritFromApp)` | `update`/`delete` on an application's managed resource. Implements `server.rbac.disableApplicationFineGrainedRBACInheritance`. |
| `(*Enforcer).Can(CanRequest) (Decision, error)` | Engine for `rbac can`: shorthand resolution, strict validation (`ErrUnknownResource`, `ErrInvalidAction`), and expansion of an empty object or `*` to `*/*` for project-scoped resources. |
| `Validate([]Source, ValidateOptions{MatchMode, Builtin})` | Engine for `rbac validate`: syntax errors plus warnings, including roles assigned by `g` lines that grant nothing (referential integrity). |
| `SetEnforcementEnabled(bool)` | Equivalent of `--disable-auth`: allows everything. |
| `PolicyCSV(data)`, `PolicySources(data)` | ConfigMap composition: `policy.csv`, then `policy.<suffix>.csv` in key order. |
| `ParseScopes`, `ScopeValues`, `UserIdentifier`, `SubjectsFromClaims` | `scopes` handling (`[groups]`, `[groups, email]`, a scalar, `[]`); `federated_claims.user_id` falls back to `sub`. |
| `ParseProjectSubject`, `ProjectFromObject`, `AppObject`, `SubResourceAction`, `CustomAction` | Object and subject helpers (`<project>/<app>`, `<project>/<ns>/<app>`, `update/<g>/<k>/<ns>/<n>`, `action/<g>/<k>/<a>`). |
| Constants | Every baseline resource (`ResourceApplications` … `ResourceExtensions`), action (`ActionGet` … `ActionInvoke`, `ActionAll`), ConfigMap key, match mode, `RoleAdmin` and `RoleReadOnly`. `BuiltinPolicy()` returns the embedded `builtin-policy.csv`. |

### Adapting CRD types (callers)

- **AppProject:** for each project, call `SetProjectPolicies(proj.Name, roles)`. Build
  `roles[i] = rbac.Role{Name: r.Name, Policies: r.Policies, Groups: r.Groups}` from
  `spec.roles`. Call it from the informer's add and update handlers, and call
  `DeleteProjectPolicies` on delete. **Register every project, including projects with no
  roles.** Token confinement uses the registry. Upstream looks the project up in a lister,
  so an unregistered project behaves like a project that does not exist.
- **Sessions:** pass `SubjectsFromClaims(claims, scopes)`, with `scopes` from
  `ParseScopes(cm["scopes"])`.
- **Project-role tokens:** pass `Subjects{ProjectRole: &rbac.ProjectRole{...}}`, and only
  after `internal/auth` has verified the token and its metadata. Never derive this from an
  SSO subject.
- **Application objects:** use `AppObject(controlPlaneNamespace, project, namespace, name)`.

## Evaluation

The policy model is `p = sub, res, act, obj, eft` and `g = member, role`. For one subject:
breadth-first role discovery through `g` lines follows at most **10 hops** (the Casbin
default role manager limit), and cycles are tolerated. The result is **allow** if some
matching rule allows and none denies. Fields are matched in the order resource, action,
object, and the first mismatch ends the comparison. An invalid regex that is reached during
evaluation fails that subject's evaluation (a deny).

`Enforce`:

1. Disabled means allow. If the last policy load failed, deny.
2. Evaluate the default role (`policy.default`) against built-in plus user policy. An allow
   is final: a deny for the user cannot remove what the default role grants.
3. Derive the project from the request: the first `/` segment of the object for
   `applications`, `applicationsets`, `logs`, `exec`, `clusters` and `repositories` when
   the object has at least two segments, or the whole object for `projects`. If that
   project is registered, its roles join the policy for this request only. Each role also
   gets the implicit rule `p, proj:<p>:<r>, projects, get, <p>, allow`.
4. Project-role token:
   - If the request belongs to a registered project other than the token's, deny.
   - If it belongs to the token's project, evaluate the default role and then
     `proj:<p>:<r>`.
   - Otherwise, evaluate `proj:<p>:<r>` as a user against the global policy.
5. If the request has project policies, re-evaluate the default role against them.
6. Evaluate the user, then each group that is the **member of some `g` line** (global or of
   that project). Each subject is evaluated independently, and the first allow wins. A deny
   for the user does not cancel an allow through a group, and the reverse also holds.

## Invariants

- Reads are lock-free. `Enforce` loads one immutable snapshot through `atomic.Pointer`.
  Writers (`SetPolicy*`, `SetProjectPolicies`, `DeleteProjectPolicies`,
  `SetEnforcementEnabled`) are serialized by a mutex and publish a new snapshot.
  `SetPolicy` is atomic across builtin, user, default role and match mode.
- Project policies only ever take part in requests whose object resolves to that project.
  Role policies must name `proj:<p>:<role>`, use a project-scoped resource and an object
  matching `^<p>(/[*\w-.]+)?/[*\w-.]+$`, and be `p` lines. So a project editor cannot add
  `g` lines, cross-project objects, or grants on other resources.
- Line order never changes a decision (property-tested).
- Adding a deny line never widens access. This is property-tested for well-formed denies,
  for broken global lines, and for valid and invalid project-role lines.
- In the steady state, `Enforce` allocates only the `Reason` string: 2 or 3 allocations,
  and `MatchedPolicy` strings are precomputed.

## Failure modes

| Condition | Behavior |
|-----------|----------|
| A built-in or user policy line fails the grammar: bad CSV, wrong arity (`p` needs 6 fields, `g` needs 3), unknown line type, a line over 16 KiB, or a policy over 16 MiB | `SetPolicy` returns a `*PolicyError` and the enforcer **fails closed**: every request is denied with reason "RBAC policy failed to load". This matches upstream, where the Casbin enforcer cannot be built. The next valid `SetPolicy` restores service. `LoadError()` exposes the state. |
| `r`, `e` or `m` lines | Accepted as no-ops, as upstream does. A warning is reported. |
| An effect other than `allow` or `deny` | The rule never takes effect (warning). |
| Invalid glob pattern | The rule never matches (warning), as upstream. |
| Invalid regex in regex mode | Any evaluation that reaches it denies (warning). |
| Invalid project role items | Invalid lines and invalid group bindings are dropped. A role with any invalid policy line keeps **only its deny rules**, and loses its implicit project `get`. An invalid role name drops the role. `SetProjectPolicies` returns a `*PolicyError` and the problems stay readable through `ProjectProblems`. Global grants are unaffected. |
| Unknown `policy.matchMode` | Treated as glob (warning). |
| Invalid `scopes` YAML | `ParseScopes` returns an error. The caller decides; the enforcer is not affected. |

## Security notes

- **Reserved subject form.** A `User` or group named `proj:<p>:<r>` is ignored, unless it
  arrives as `Subjects.ProjectRole`. This prevents an SSO identity from impersonating a
  project role. Upstream treats such strings as project tokens.
- **Group/local-account collision.** This is upstream parity, documented upstream: an SSO
  group named like a local account (for example `admin`) inherits that account's `g`
  bindings. A test pins this behavior.
- **What is logged.** Policy text and decisions are not secrets. The enforcer logs only
  policy-load events, never individual requests. `Decision.Reason` contains subject names
  and is meant for audit records.
- **Untrusted input bounds.** Project role policies come from project editors. They are
  bounded by line size, and pattern compilation is deduplicated. Glob matching uses
  gobwas/glob's backtracking matcher, and pattern length is capped by the line limit.
  Regex uses RE2, which matches in linear time.

## Parity feature areas served

From `docs/parity/areas/07-projects-rbac-auth.md`:

- **Fully provided by this package's engine:** TEN-065 (model and pipeline), TEN-066
  (grammar), TEN-067 (resources), TEN-068 (actions, as tokens), TEN-069 (object syntax),
  TEN-070 and TEN-071 (fine-grained actions and the inheritance switch, through
  `EnforceAppResource`), TEN-072 (deny precedence), TEN-073 (default role), TEN-074 (scopes
  and groups), TEN-075 (match modes), TEN-076 (built-in roles), TEN-077 (overlays),
  TEN-079 (runtime project policy merge), TEN-080 (validation and referential integrity),
  TEN-083 (`write-repositories`).
- **Rule engine only:** TEN-017 to TEN-020 (role, policy and group validation; runtime
  policy generation) and TEN-036 (project token authorization).
- **Engine for features wired elsewhere:** TEN-078 (live reload), TEN-081 (error text),
  TEN-084 (logs, exec and extensions), TEN-085 (`--disable-auth`), TEN-087 and TEN-088
  (CLI), TEN-089 (`can-i`). The API server, CLI and informer code for these belongs to
  other packages.

## Intentional differences from upstream

1. **`rbac can --strict` resource/action table.** Patrol's table is a superset: it adds
   `write-repositories` CRUD and `accounts get`. Upstream rejects both (TEN-087), although
   both are baseline grants.
2. **Invalid project roles.** Upstream drops the whole runtime policy when it fails to
   parse, and loads semantically invalid lines verbatim. Admission validation normally
   prevents both cases. Patrol validates each line instead and reduces a broken role to its
   denies, so a broken project never grants more than a fixed one.
3. **Reserved `proj:` names** for users and groups (see Security notes).
4. **`scopes` as a scalar.** The upstream docs say a plain string is accepted; the upstream
   code rejects it. Patrol accepts it as a one-element list.
5. **Glob library version.** gobwas/glob v1.0.0 (upstream uses v0.2.3). The syntax is the
   same; edge-case bug fixes in v1 may differ. Fast paths (all-`*` and literal patterns)
   are property-tested against the library.
6. **Empty subject.** Upstream also evaluates the empty string as a subject when the claims
   enforcer fails. Patrol does not; only the default role applies to `Subjects{}`.

## Known gaps

- **Regex mode with the built-in policy.** The built-in policy's `*` and `*/*` patterns are
  invalid regular expressions, so in `policy.matchMode: regex` the built-in roles deny.
  Patrol reproduces Casbin's error-as-deny behavior. This was derived from reading the code
  only: the Casbin source is not in the research checkout (listed as Unconfirmed in the
  parity area). Validation reports these lines as warnings.
- **Unregistered projects.** Project-token confinement depends on the caller registering
  every AppProject (see "Adapting CRD types").
- **Not in this package:** ConfigMap and AppProject informer wiring, project token
  revocation checks, audit events, and RFC 9457 mapping of `PermissionDeniedError`.
- **Glob cache size (TEN-086).** There is no equivalent setting: compiled patterns live
  with the policy that owns them, so no global cache exists.

## Performance

`BenchmarkEnforce10kLines` uses 10,000 policy lines: 2,000 team roles, each with four `p`
lines and one `g` binding, plus the built-in policy. Glob mode measured 1.0 to 2.7 µs per
`Enforce` with 2 or 3 allocations. `BenchmarkParse10kLines` measured about 74 to 95 ms per
parse of 10,000 lines.

These are single runs on an Apple M1 Pro that was heavily shared: load average 175 to 260
across 10 cores during every run. Timings varied up to 5 times between runs; allocation
counts were stable. Rerun on an idle machine before quoting these numbers:

```
go test ./internal/rbac/ -run '^$' -bench . -benchtime=1s
```

## Verification

```
go build ./...
go vet ./internal/rbac/...
go test -race -count=1 ./internal/rbac/...
```

The tests are table-driven ports of the upstream semantics:

- built-in roles, wildcards and deny precedence, project isolation, URL objects, default
  role, glob, regex and invalid patterns, action actions, policy updates, fail-closed
  loading, `--disable-auth`, error text, ConfigMap overlays and scopes;
- project role scoping, tokens, apps in any namespace, validation messages, fine-grained
  sub-resources with inheritance on and off, `rbac can` and `validate`.

The `rapid` properties are:

- adding a deny never widens access;
- line order is irrelevant;
- the indexed evaluator agrees with a naive Casbin-shaped oracle built directly on
  glob and regexp;
- pattern fast paths agree with the libraries;
- the CSV fast path agrees with `encoding/csv`;
- ConfigMap composition gives the same result as a single CSV or as named sources.
