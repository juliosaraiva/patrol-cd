# Component: `internal/rbac`

Status: implemented (phase 1, library only). Owner task: T1.2. Decision record: ADR-0009.

Patrol's own evaluator for the Casbin-style RBAC grammar used by Argo CD v3.5.5. There is no
Casbin dependency. Matching uses `github.com/gobwas/glob` (glob mode) and Go `regexp` (regex
mode). Glob patterns that contain an alternation (`{a,b}`) are translated into an equivalent
RE2 expression instead of being matched by gobwas/glob (see Security notes). The package has
no Kubernetes or CRD dependency, and it does not import `api/v1alpha1`.

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
(`internal/apiserver/rest/audit.go`), and HTTP error mapping (`internal/apiserver`).

## Public API

| Symbol | Purpose |
|--------|---------|
| `Parse(csv, ParseOptions) (*Policy, []ValidationError, error)` | Parses one document as a user source. `ParseOptions{MatchMode, Source, MaxBytes, MaxLineBytes}`. On any error-severity problem the policy is nil and `err` is a `*PolicyError` (`errors.Is(err, ErrInvalidPolicy)`). |
| `ParseSources([]Source, ParseOptions)` | Same as `Parse`, but for several named documents (for example `policy.csv` and `policy.x.csv`). Each problem keeps the line number from its own source. `Source{Name, CSV, Builtin}`: `Builtin` marks the built-in policy, the only source in which a pattern that cannot be evaluated is a warning instead of an error (see "Pattern validation"). |
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
object, and the first mismatch ends the comparison. Subjects are not patterns: rules are
looked up by exact subject name, as Casbin's `g` function does. Patterns are checked when
a policy loads (see "Pattern validation"), so only built-in rules can still fail during
evaluation; reaching one fails that subject's evaluation (a deny).

`Enforce`:

1. Disabled means allow. If the last policy load failed, deny.
2. Evaluate the default role (`policy.default`) against built-in plus user policy. An allow
   is final: a deny for the user cannot remove what the default role grants.
3. Derive the project from the request: the first `/` segment of the object for
   `applications`, `applicationsets`, `logs`, `exec`, `clusters` and `repositories` when
   the object has at least two segments, or the whole object for `projects`. If that
   project is registered, its roles join the policy for this request only. Each role also
   gets the implicit rule `p, proj:<p>:<r>, projects, get, <p>, allow`, whose object
   matches the project name exactly (it is not compiled as a pattern).
4. Project-role token:
   - If the request belongs to a registered project other than the token's, deny.
   - If it belongs to the token's project, evaluate the default role and then
     `proj:<p>:<r>`.
   - Otherwise, evaluate `proj:<p>:<r>` as a user against the global policy.
5. If the request has project policies, re-evaluate the default role against them.
6. Evaluate the user, then each group that is the **member of some `g` line** (global or of
   that project). Each subject is evaluated independently, and the first allow wins. A deny
   for the user does not cancel an allow through a group, and the reverse also holds.

## Pattern validation

Each resource, action and object pattern is compiled once, when its policy loads, by
`compilePattern`. Evaluation only runs the compiled matchers, so the load-time check sees
exactly what evaluation will run. A pattern that does not compile is handled by where it
comes from:

| Source | Invalid glob (glob mode) | Pattern that cannot be evaluated: invalid regular expression (regex mode), or a glob alternation without an exact RE2 equivalent |
|--------|--------------------------|------------------------------------------------------------------|
| Built-in policy (`Source.Builtin`) | Warning. The rule never matches, as upstream. | Warning. Reaching the rule fails the evaluation of that subject (see below), as upstream. In regex mode this covers the built-in `*` and `*/*` patterns. |
| User policy (`policy.csv` and overlays) | Warning. The rule never matches, as upstream. | **Error.** `SetPolicy` returns a `*PolicyError` and the enforcer fails closed until a valid policy loads. `Validate` reports the same error. |
| Project role line | **Invalid line.** | **Invalid line.** |
| Implicit project `get` rule | Not compiled: exact match. | Not compiled: exact match. |

An invalid project role line is handled like every other invalid project line: it is
ignored, its role keeps only its deny rules (and loses its implicit project `get`), and the
problem is returned by `SetProjectPolicies` and `ProjectProblems` with the project, role
and line number (`project/<p>/role/<r>:<line>`). Examples in regex mode: action `*`,
`action/[` or `update/(`, object `<p>/**`. In glob mode: action `action/[` or
`update/{x`. The resource must already be a literal project-scoped resource and the
subject must be `proj:<p>:<r>`, so invalid patterns there are rejected by those rules
first. Objects only allow `*`, word characters, `-` and `.` after the project name, so a
broken object needs `**` (regex mode) or metacharacters in the project name.

Changing `policy.matchMode` recompiles and revalidates every registered project, and logs
the problems. A role written for glob mode (`applications, *, <p>/*`) is therefore reduced
to its denies in regex mode, and recovers when the mode changes back.

Consequence: **no project role rule and no user policy rule can fail during evaluation.**
Only rules of the built-in source can (in practice the built-in policy in regex mode, or a
custom built-in policy passed to `SetPolicy`).

### Evaluation errors (built-in rules only)

A rule is reached when its subject is the evaluated subject or one of its roles, and the
fields before the broken one match. Reaching a rule whose pattern cannot be evaluated
fails the evaluation of **that subject for that request**: the user, one group, or the
default role is denied (`Decision.Reason` "policy evaluation failed for …", and
`MatchedPolicy` names the rule). Other subjects of the same request (other groups, the
default role) are still evaluated independently, and other requests are unaffected.

The failure is deliberately **not isolated to the rule** (treating the rule as a
non-match). For a failing deny that would silently widen access: the deny would stop
applying and an allow next to it would go through. For a failing allow it would change the
documented Casbin error-as-deny behavior of the built-in roles in regex mode. A project
editor cannot route anyone into such a rule: project bindings only link groups to
`proj:<p>:<r>`, whose own rules cannot fail, and only the global policy (written by
administrators) can link a project role onward.

## Invariants

- Reads are lock-free. `Enforce` loads one immutable snapshot through `atomic.Pointer`.
  Writers (`SetPolicy*`, `SetProjectPolicies`, `DeleteProjectPolicies`,
  `SetEnforcementEnabled`) are serialized by a mutex and publish a new snapshot.
  `SetPolicy` is atomic across builtin, user, default role and match mode.
- Project policies only ever take part in requests whose object resolves to that project.
  Role policies must name `proj:<p>:<role>`, use a project-scoped resource and an object
  matching `^<p>(/[*\w-.]+)?/[*\w-.]+$`, and be `p` lines. So a project editor cannot add
  `g` lines, cross-project objects, or grants on other resources.
- A project role policy line contains exactly five commas, as upstream requires (it splits
  the raw line on `,` before any CSV unquoting). No project policy field can contain a
  comma, even when quoted, so project editors cannot write glob alternations.
- Pattern matching never backtracks exponentially. Each rule field costs
  O(len(pattern) × len(value)): gobwas/glob for patterns without alternations, RE2 for glob
  patterns with alternations and for regex mode.
- Line order never changes a decision, for global lines and for project role lines
  (property-tested).
- Adding a deny line never widens access. This is property-tested for well-formed denies,
  for broken global lines, and for valid and invalid project-role lines, including lines
  whose patterns do not compile in the match mode (`*`, `[`, `(`, `**`).
- No project role rule and no user policy rule can fail during evaluation (see "Pattern
  validation"). Property-tested: no compiled project rule contains a pattern that does not
  compile, and no decision is an evaluation failure.
- A project role's allow lines never deny anything, valid or not (property-tested). A
  project role can only take away access through its own valid deny lines, and only for
  requests in its project.
- Project-role confinement (property-tested, with invalid project lines): a project's roles
  never change a decision for a request outside the project, and a project token gets
  nothing in another registered project beyond what the default role grants.
- In the steady state, `Enforce` allocates only the `Reason` string: 2 or 3 allocations,
  and `MatchedPolicy` strings are precomputed.

## Failure modes

| Condition | Behavior |
|-----------|----------|
| A built-in or user policy line fails the grammar: bad CSV, wrong arity (`p` needs 6 fields, `g` needs 3), unknown line type, a line over 16 KiB, or a policy over 16 MiB | `SetPolicy` returns a `*PolicyError` and the enforcer **fails closed**: every request is denied with reason "RBAC policy failed to load". This matches upstream, where the Casbin enforcer cannot be built. The next valid `SetPolicy` restores service. `LoadError()` exposes the state. |
| `r`, `e` or `m` lines | Accepted as no-ops, as upstream does. A warning is reported. |
| An effect other than `allow` or `deny` | The rule never takes effect (warning). |
| Invalid glob pattern in the built-in or user policy | The rule never matches (warning), as upstream. |
| Invalid regex in regex mode, in the user policy | Error: the policy does not load and the enforcer fails closed (see the first row). |
| Invalid regex in regex mode, in the built-in policy | Warning. Any evaluation that reaches it denies that subject (see "Evaluation errors"). |
| Glob alternation pattern without an exact RE2 equivalent: a literal U+FFFD inside a pattern with alternations, or alternations nested deeper than RE2's limit of 1000 levels | User policy: error, as for an invalid regex. Built-in policy: warning "cannot be evaluated", and any evaluation that reaches it denies. The backtracking matcher is never used for alternations. |
| Project role line whose resource, action or object does not compile in the active match mode (regex: `*`, `action/[`, `update/(`, `<p>/**`; glob: `action/[`) | The line is invalid. The role keeps only its denies (see the next rows). |
| Project role policy line whose raw text does not have exactly five commas (for example a quoted field containing a comma) | The line is invalid ("exactly six comma-separated fields"), like upstream. The role keeps only its denies (see the next row). |
| Invalid project role items | Invalid lines and invalid group bindings are dropped. A role with any invalid policy line keeps **only its deny rules**, and loses its implicit project `get`. An invalid role name drops the role. `SetProjectPolicies` returns a `*PolicyError` and the problems stay readable through `ProjectProblems`. Global grants are unaffected. |
| `policy.matchMode` changes | Every registered project is recompiled and revalidated for the new mode; new problems are logged and replace those in `ProjectProblems`. |
| Unknown `policy.matchMode` | Treated as glob (warning). |
| Invalid `scopes` YAML | `ParseScopes` returns an error. The caller decides; the enforcer is not affected. |

## Security notes

- **Project role lines cannot fail during evaluation.** Before this was enforced, a regex
  mode project line such as `p, proj:p1:r, applications, *, p1/.*, allow` passed the
  AppProject rules (`*` is a valid role action) but was not a valid regular expression.
  Every evaluation that reached it failed, so every member of the role's groups was denied
  every `applications` request in `p1`, global grants included. Because a project role can
  bind any group name (`Role.Groups`), a project editor could do this to another team's
  group, or to a global role such as `role:admin`. It failed closed, so it could not grant
  access. Adding any other invalid line to the role reduced it to its denies and removed the
  effect, which is how the property "adding a deny never widens access" found it. Lines are
  now compiled when the project is registered, and a line that does not compile is invalid
  (see "Pattern validation"). Regression test:
  `TestRegressionProjectRegexLineDoesNotPoisonGlobalGrant`; the original rapid fail file is
  kept under `internal/rbac/testdata/rapid/` and replayed by every test run.

- **Reserved subject form.** A `User` or group named `proj:<p>:<r>` is ignored, unless it
  arrives as `Subjects.ProjectRole`. This prevents an SSO identity from impersonating a
  project role. Upstream treats such strings as project tokens.
- **Group/local-account collision.** This is upstream parity, documented upstream: an SSO
  group named like a local account (for example `admin`) inherits that account's `g`
  bindings. A test pins this behavior.
- **What is logged.** Policy text and decisions are not secrets. The enforcer logs only
  policy-load events, never individual requests. `Decision.Reason` contains subject names
  and is meant for audit records.
- **Matching cost and untrusted patterns.** gobwas/glob matches by backtracking. With
  alternation groups whose branches contain `*` (for example `{*a,*aa}{*a,*aa}…b*`), its
  running time grows exponentially with the number of groups and the value length. In a
  local probe, four such groups against a 47-byte value took 0.2 s. A run over two to eight
  groups against a 67-byte value did not finish within 250 s. A pattern length limit does
  not bound this. Two independent defenses apply:
  1. **Project role policies cannot contain alternations.** Like upstream `validatePolicy`,
     a project role line must split on `,` into exactly six parts before CSV unquoting.
     So no field can contain a comma, and a glob group without a comma is a plain sequence.
     Without separators, each gobwas/glob `*` drops all pending restart points when it
     stores its own. Matching comma-free patterns is therefore
     O(len(pattern) × len(value)). In the same probe, near-worst-case comma-free patterns
     of up to 16 KiB against values of up to 16 KiB took at most about 1 ms each.
  2. **The engine never backtracks over alternations, whatever the source.** Any glob
     pattern with a `,` inside `{…}` (from `policy.csv`, an overlay or the built-in policy)
     is translated into an anchored RE2 expression. The translation uses gobwas/glob's own
     lexer, and a property test checks it against the library, including on newlines,
     invalid UTF-8 and U+FFFD. RE2 matches in O(len(pattern) × len(value)). Patterns
     without an exact RE2 equivalent fail closed (see Failure modes).

  Cost per rule is still proportional to the value length. Values are request fields
  (resource, action, object). The API layer should keep them within Kubernetes name
  limits, as legitimate requests do. Lines are capped at 16 KiB and pattern compilation is
  deduplicated. `Enforce` and `Can` do not impose their own value length cap.

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
7. **Glob alternations use RE2.** Upstream matches them with gobwas/glob's backtracking.
   Patrol's results are identical (property-tested), except for the two patterns listed
   under Failure modes, which deny instead of matching (built-in policy) or are rejected
   (user policy). Matching takes polynomial time, never exponential time.
8. **Patterns that cannot be evaluated are rejected when they load.** Upstream (Casbin)
   loads them and errors during evaluation; the error denies the evaluated subject
   everything that reaches the rule. Patrol keeps that behavior only for the built-in
   policy. A user policy with such a pattern does not load (the enforcer fails closed and
   `rbac validate` reports an error; `rbac can` exits with "invalid user policy" where
   upstream prints "No"). A project role line with such a pattern, or with an invalid glob
   (upstream loads it as a rule that never matches), is invalid and reduces its role to
   its denies.
9. **Implicit project rule.** Its object matches the project name exactly. Upstream
   compiles the name as a pattern. The two agree for every valid Kubernetes name (the
   only requests that reach the rule have the project name as their object); they differ
   only for names with pattern metacharacters, where upstream's rule could fail (regex
   mode) or never match.

## Known gaps

- **Regex mode with the built-in policy.** The built-in policy's `*` and `*/*` patterns are
  invalid regular expressions, so in `policy.matchMode: regex` the built-in roles deny.
  Patrol reproduces Casbin's error-as-deny behavior for these rules only (see "Evaluation
  errors"). This was derived from reading the code only: the Casbin source is not in the
  research checkout (listed as Unconfirmed in the parity area). Validation reports these
  lines as warnings.
- **Project roles written for glob mode.** In regex mode the role action `*` is invalid,
  and the AppProject action rules (upstream parity) do not accept `.*`, so a project role
  cannot grant every action with one line in regex mode; it has to list the actions.
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
counts were stable.

Load-time pattern validation (2026-10-16) does not touch the `Enforce` path; the only
compiled-rule change is the implicit project rule's exact matcher. Same machine, load
average 13 to 27, `go test -run xxx -bench . ./internal/rbac/`, before then after:
`Enforce` 248 to 506 ns before and 253 to 510 ns after across the ten
`BenchmarkEnforce10kLines` cases (each case within 4%), 2 or 3 allocations in both;
`BenchmarkParse10kLines` 8.8 ms then 9.3 ms with identical allocations (121,904); an
interleaved A/B of the old and new per-line severity logic gave overlapping ranges (old
9.9 to 18.0 ms, new 9.5 to 13.7 ms, six runs each).
`BenchmarkEnforceParallel` measured 72 ns before and 89 ns after, and 92 to 130 ns across
four repeated runs after, so it is dominated by machine load. Rerun on an idle machine
before quoting these numbers:

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
  sub-resources with inheritance on and off, `rbac can` and `validate`;
- matching cost (`complexity_test.go`): commas in project role fields are rejected, and the
  review's adversarial project policy no longer stalls `Enforce`. Comma-free near-worst-case
  project patterns and global alternation patterns finish within a deadline, with
  semantics checked. Untranslatable alternations are rejected in user sources and fail
  closed in the built-in source;
- pattern validation (`pattern_validation_test.go`): the regression for the regex-mode
  project line, invalid regular expressions and globs in each field position of a project
  line (both modes), the exact implicit project rule, revalidation on a match mode change,
  fail-closed built-in rules (a failing deny is not skipped), and `Validate` severities.
  `parse_test.go` covers user versus built-in severities for each field position.

The `rapid` properties are (project role generators include fields that do not compile
in the drawn mode: `*`, `[`, `(` and `**`):

- adding a deny never widens access (its saved fail file is replayed on every run);
- line order is irrelevant, for global and project role lines;
- project-role confinement, and no compiled project rule or decision that can fail;
- a project role's allow lines, valid or not, never deny anything;
- the indexed evaluator agrees with a naive Casbin-shaped oracle built directly on
  glob and regexp (policies with invalid regular expressions are rejected as user sources
  and compared as built-in sources, where both fail closed);
- pattern fast paths agree with the libraries;
- the RE2 translation of glob alternations agrees with gobwas/glob, including on
  newlines, invalid UTF-8 and U+FFFD;
- the CSV fast path agrees with `encoding/csv`;
- ConfigMap composition gives the same result as a single CSV or as named sources.
