---
name: review-change
description: Agent code review of the current Patrol CD branch before a push. Runs the review-change workflow (reviewer agents chosen by the changed paths, then an adversarial verifier for every blocker or major finding) and records the outcome for HEAD's tree, which the pre-push gate requires. Use after `make sdlc-check` passed and before `git push`, or when asked to review the branch.
---

# Review the change before pushing

The push gate (`hack/sdlc/gate.sh`, run by the Claude Code hook on every `git push`) needs a review
record for the tree being pushed. This skill produces it. Follow the steps in order; do not skip
the workflow or write a record by hand.

1. **Commit first.** The review is tied to HEAD's tree. Run:

   ```bash
   hack/sdlc/review-input.sh
   ```

   It refuses a dirty tree. It prints a JSON object (`repo`, `base`, `head`, `tree`, `docsOnly`,
   `diffFile`, `files`) and writes the diff to `diffFile`.

2. **Documentation only?** If `docsOnly` is `true`, no review is needed: the gate accepts
   documentation-only changes with the check record alone. Stop here.

3. **Run the workflow.** Call the Workflow tool with
   `scriptPath: ".claude/workflows/sdlc-review.js"` and `args` set to the JSON object from step 1
   (as an object, not a string). It selects the reviewers from `.claude/agents/` by path, runs them
   in parallel, and sends every blocker and major finding to the `review-verifier`, which tries to
   refute it. Wait for it to finish.

4. **Write the report** to `<diffFile without .diff>.md` (same directory, named after the tree):
   - the tree, base and head, and the reviewers that ran with their `covered` notes;
   - every confirmed blocking finding (`blocking`): reviewer, severity, file:line, title,
     scenario, evidence, fix, and the verifier's reason;
   - the minor findings, one line each;
   - the refuted findings, one line each with the verifier's reason;
   - `incomplete` reviewers, if any.

5. **Record it.** `N` is the number of confirmed blocking findings. If `incomplete` is not empty,
   the review did not finish: do not record a pass; run the workflow again (or record it with
   `N` at least 1 so the gate keeps blocking).

   ```bash
   hack/sdlc/record-review.sh --tree <tree> --blocking <N> --report <report.md>
   ```

6. **Act on it.**
   - `N` > 0: fix every blocking finding, commit, run `make sdlc-check` again and this skill again
     (the new commit has a new tree; old records do not carry over). Do not argue findings away in
     the record: if a verified finding is wrong, say why in the report and in the pull request, and
     let the owner decide; the gate stays closed until a review of the new tree passes.
   - `N` = 0: push. Put the minor findings that you did not fix in the pull request description
     (or file issues for them, labelled as `TASKS.md` describes).

Findings and reports may contain file paths and code; they never contain secrets. A vulnerability
found by the review goes to a private report (`SECURITY.md`), not into the pull request text.
