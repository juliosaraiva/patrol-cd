export const meta = {
  name: 'sdlc-review',
  description: 'Agent review of the current branch before a push: path-selected Patrol CD reviewers, then an adversarial verifier per blocking finding',
  whenToUse: 'Only through the review-change skill, which prepares its input and records its outcome (docs/guides/claude-code-sdlc.md); do not run it directly',
  phases: [
    { title: 'Review', detail: 'one reviewer per area the change touches (.claude/agents)' },
    { title: 'Verify', detail: 'review-verifier tries to refute each blocker and major finding' },
  ],
}

// args: the JSON printed by hack/sdlc/review-input.sh
// { repo, base, head, tree, docsOnly, diffFile, files: [...] }
const input = typeof args === 'string' ? JSON.parse(args) : args
if (!input || !input.tree || !Array.isArray(input.files)) {
  throw new Error('sdlc-review: run it through the review-change skill, which passes the output of hack/sdlc/review-input.sh as args')
}

// Which reviewers see which files. A reviewer runs when at least one changed file matches; it gets
// the matching files as its focus and the whole diff for context.
const REVIEWERS = [
  { name: 'go-reviewer', match: /\.go$/ },
  { name: 'controller-reviewer', match: /^(api\/|internal\/(controller|appset|cache|sync|hydrator|commitserver|clusters|settings|project|tracking|compare|diff|health|server|notifications\/controller)\/).*\.go$/ },
  { name: 'security-reviewer', match: /^(internal\/(apiserver|auth|rbac|reposerver|render|cmp|git|helm|oci|sandbox|webhook|lua|server|cli\/sso)\/|pkg\/|cmd\/|deploy\/|Dockerfile$|hack\/images\/|\.github\/)/ },
  { name: 'test-reviewer', match: /(_test\.go$|^test\/|\/testdata\/|^hack\/ci\/|\.test\.tsx?$|^ui\/e2e\/)/ },
  { name: 'api-reviewer', match: /^(internal\/apiserver\/|pkg\/apiclient\/|api\/openapi\/|internal\/cli\/|cmd\/patrol\/|ui\/src\/api\/|docs\/parity\/)/ },
  { name: 'ui-reviewer', match: /^ui\// },
  { name: 'delivery-reviewer', match: /^(deploy\/|Dockerfile$|\.dockerignore$|Makefile$|hack\/[^/]+\.mk$|hack\/(images|kind|ci|release|sdlc)\/|test\/e2e\/|\.github\/|\.githooks\/|\.claude\/)/ },
]

const FINDINGS = {
  type: 'object',
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          file: { type: 'string' },
          line: { type: 'integer' },
          severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
          title: { type: 'string', description: 'one line' },
          scenario: { type: 'string', description: 'the input, state, event sequence or request that makes it fail, and what happens' },
          evidence: { type: 'string', description: 'what you read or ran that shows it' },
          fix: { type: 'string' },
        },
        required: ['file', 'line', 'severity', 'title', 'scenario', 'evidence', 'fix'],
      },
    },
    covered: { type: 'string', description: 'what you reviewed and found sound, in two or three sentences' },
  },
  required: ['findings', 'covered'],
}
const VERDICT = {
  type: 'object',
  properties: {
    real: { type: 'boolean' },
    severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
    reason: { type: 'string', description: 'what you read or ran, and why the finding holds or not' },
  },
  required: ['real', 'severity', 'reason'],
}

const common = `Repository: ${input.repo} (Patrol CD). Change under review: ${input.base}..${input.head} (tree ${input.tree}); the full diff is in ${input.diffFile}. Read-only: never edit files in the repository, never commit, push, or run gh commands with side effects. Throwaway tests go through \`go test -overlay\` from a temporary directory.`

if (input.docsOnly) {
  log('documentation-only change: no agent review needed')
  return { tree: input.tree, reviewers: [], blocking: [], minor: [], refuted: [], notes: 'documentation only' }
}

let selected = REVIEWERS.map(r => ({ ...r, files: input.files.filter(f => r.match.test(f)) })).filter(r => r.files.length > 0)
if (selected.length === 0) {
  // Not documentation, but nothing a specialist owns (examples/, scripts outside hack/...): one
  // generalist pass.
  selected = [{ name: 'go-reviewer', files: input.files }, { name: 'delivery-reviewer', files: input.files }]
}
log(`reviewers: ${selected.map(r => `${r.name} (${r.files.length})`).join(', ')}`)

const results = await pipeline(
  selected,
  r => agent(
    `${common}\n\nYou are the ${r.name}. Read .claude/agents/${r.name}.md in the repository first and follow it exactly.\n\nFiles of this change in your area (review these; use the rest of the diff for context):\n${r.files.map(f => `- ${f}`).join('\n')}\n\nReport every defect you can prove; an empty list is a valid answer.`,
    { label: `review:${r.name}`, phase: 'Review', schema: FINDINGS },
  ),
  (out, r) => {
    const found = (out ? out.findings : []).map(f => ({ ...f, reviewer: r.name }))
    const minor = found.filter(f => f.severity === 'minor')
    const toVerify = found.filter(f => f.severity !== 'minor')
    return parallel(toVerify.map(f => () => agent(
      `${common}\n\nYou are the review-verifier. Read .claude/agents/review-verifier.md in the repository first and follow it exactly.\n\nThe ${r.name} reported this finding:\n${JSON.stringify(f, null, 1)}`,
      { label: `verify:${r.name}:${f.file}:${f.line}`, phase: 'Verify', schema: VERDICT },
    ).then(v => ({ ...f, verdict: v })))).then(vs => ({
      reviewer: r.name,
      covered: out ? out.covered : 'the reviewer returned nothing',
      failed: !out,
      minor,
      verified: vs.filter(Boolean),
      unverified: toVerify.length - vs.filter(Boolean).length,
    }))
  },
)

const per = results.filter(Boolean)
const blocking = []
const minor = []
const refuted = []
for (const r of per) {
  minor.push(...r.minor)
  for (const f of r.verified) {
    if (!f.verdict.real) refuted.push(f)
    else if (f.verdict.severity === 'minor') minor.push({ ...f, severity: 'minor' })
    else blocking.push({ ...f, severity: f.verdict.severity })
  }
}
// A reviewer or verifier that died leaves the review incomplete: say so, the skill must not record a pass.
const incomplete = per.filter(r => r.failed || r.unverified > 0).map(r => r.reviewer)
  .concat(selected.filter(s => !per.find(r => r.reviewer === s.name)).map(s => s.name))
log(`blocking ${blocking.length}, minor ${minor.length}, refuted ${refuted.length}${incomplete.length ? `, INCOMPLETE: ${incomplete.join(', ')}` : ''}`)
return {
  tree: input.tree,
  reviewers: per.map(r => ({ name: r.reviewer, covered: r.covered })),
  blocking,
  minor,
  refuted,
  incomplete,
}
