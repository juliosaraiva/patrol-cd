package retention

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func op(name string, created int, phase v1alpha1.OperationPhase) v1alpha1.Operation {
	return v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(time.Unix(int64(created), 0))},
		Status:     v1alpha1.OperationStatus{Phase: phase},
	}
}

func names(ops []v1alpha1.Operation) []string {
	out := []string{}
	for _, o := range ops {
		out = append(out, o.Name)
	}
	return out
}

func TestSelect(t *testing.T) {
	t.Parallel()
	ops := []v1alpha1.Operation{
		op("e", 5, v1alpha1.OperationSucceeded),
		op("a", 1, v1alpha1.OperationFailed),
		op("c", 3, v1alpha1.OperationRunning),
		op("b", 2, v1alpha1.OperationSucceeded),
		op("d", 4, v1alpha1.OperationRejected),
	}
	for _, tc := range []struct {
		name  string
		limit int
		keep  string
		want  []string
	}{
		{"under the limit", 10, "", []string{}},
		{"exactly the limit", 4, "", []string{}},
		{"oldest terminal first", 2, "", []string{"a", "b"}},
		{"non-terminal never selected", 0, "", []string{"a", "b", "d", "e"}},
		{"kept operation survives", 0, "e", []string{"a", "b", "d"}},
		{"negative limit is zero", -3, "", []string{"a", "b", "d", "e"}},
		{"keeps only the newest", 1, "", []string{"a", "b", "d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, names(Select(ops, tc.limit, tc.keep)))
		})
	}
}

// TestSelectProperties checks the retention invariants on random inputs: the kept terminal
// Operations are the newest ones, at most limit of them (plus keep), and non-terminal
// Operations are never deleted.
func TestSelectProperties(t *testing.T) {
	t.Parallel()
	phases := []v1alpha1.OperationPhase{"", v1alpha1.OperationPending, v1alpha1.OperationRunning, v1alpha1.OperationTerminating,
		v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError, v1alpha1.OperationCancelled, v1alpha1.OperationRejected}
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 30).Draw(t, "n")
		limit := rapid.IntRange(-1, 12).Draw(t, "limit")
		var ops []v1alpha1.Operation
		for i := range n {
			ops = append(ops, op(fmt.Sprintf("op-%02d", i), rapid.IntRange(0, 5).Draw(t, "ts"), rapid.SampledFrom(phases).Draw(t, "phase")))
		}
		keep := ""
		if n > 0 && rapid.Bool().Draw(t, "keep") {
			keep = ops[rapid.IntRange(0, n-1).Draw(t, "keepIdx")].Name
		}
		del := Select(ops, limit, keep)
		deleted := map[string]bool{}
		for _, d := range del {
			deleted[d.Name] = true
			if !d.Status.Phase.IsTerminal() {
				t.Fatalf("non-terminal operation %s selected", d.Name)
			}
			if d.Name == keep {
				t.Fatalf("kept operation %s selected", keep)
			}
		}
		terminalLeft := 0
		for _, o := range ops {
			if o.Status.Phase.IsTerminal() && !deleted[o.Name] && o.Name != keep {
				terminalLeft++
				for _, d := range del {
					if !olderOrEqual(d, o) {
						t.Fatalf("deleted %s is newer than kept %s", d.Name, o.Name)
					}
				}
			}
		}
		lim := max(limit, 0)
		if terminalLeft > lim {
			t.Fatalf("%d terminal operations kept, limit %d", terminalLeft, lim)
		}
	})
}

func olderOrEqual(a, b v1alpha1.Operation) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name <= b.Name
}
