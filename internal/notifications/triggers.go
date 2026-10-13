package notifications

import (
	"crypto/sha1" //nolint:gosec // condition fingerprint for state keys (baseline format), not a security boundary
	"encoding/base64"
	"fmt"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// ConditionResult is the outcome of evaluating one trigger condition.
type ConditionResult struct {
	// Key identifies the condition: "[<index>].<base64url(sha1(when))>". Editing a
	// condition expression changes its key, which re-arms deliveries.
	Key string `json:"key"`
	// Triggered is true when the condition evaluated to boolean true.
	Triggered bool `json:"triggered"`
	// Templates are the templates to send.
	Templates []string `json:"templates"`
	// OncePer is the formatted oncePer value ("" when the condition has no oncePer or it
	// failed to evaluate).
	OncePer string `json:"oncePer,omitempty"`
	// Description is the condition description.
	Description string `json:"description,omitempty"`
	// Err is the evaluation error, if any. Like the baseline, an evaluation error makes the
	// condition not triggered; it is exposed for diagnostics (trigger run).
	Err error `json:"-"`
}

type compiledCondition struct {
	Condition
	key     string
	when    *vm.Program
	oncePer *vm.Program
	err     error
}

type compiledTrigger struct {
	name       string
	conditions []compiledCondition
}

func conditionHash(when string) string {
	h := sha1.Sum([]byte(when)) //nolint:gosec // see import comment
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func compileTrigger(name string, conds []Condition) *compiledTrigger {
	t := &compiledTrigger{name: name}
	for i, c := range conds {
		cc := compiledCondition{Condition: c, key: fmt.Sprintf("[%d].%s", i, conditionHash(c.When))}
		prog, err := expr.Compile(c.When)
		if err != nil {
			cc.err = fmt.Errorf("condition %d: compiling when %q: %w", i, c.When, err)
		} else {
			cc.when = prog
		}
		if c.OncePer != "" && cc.err == nil {
			p, err := expr.Compile(c.OncePer)
			if err != nil {
				cc.err = fmt.Errorf("condition %d: compiling oncePer %q: %w", i, c.OncePer, err)
			} else {
				cc.oncePer = p
			}
		}
		t.conditions = append(t.conditions, cc)
	}
	return t
}

// run evaluates every condition against vars.
func (t *compiledTrigger) run(vars map[string]any) []ConditionResult {
	out := make([]ConditionResult, 0, len(t.conditions))
	for _, c := range t.conditions {
		res := ConditionResult{Key: c.key, Templates: c.Send, Description: c.Description}
		if c.err != nil {
			res.Err = c.err
			out = append(out, res)
			continue
		}
		v, err := expr.Run(c.when, vars)
		if err != nil {
			res.Err = err
		} else if b, ok := v.(bool); ok {
			res.Triggered = b
		} else {
			res.Err = fmt.Errorf("condition %q returned %T, not bool", c.When, v)
		}
		if c.oncePer != nil {
			if ov, err := expr.Run(c.oncePer, vars); err == nil {
				res.OncePer = fmt.Sprintf("%v", ov)
			}
		}
		out = append(out, res)
	}
	return out
}
