package sync

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

var propKinds = []struct{ apiVersion, kind string }{
	{"v1", "ConfigMap"}, {"v1", "Secret"}, {"v1", "Service"}, {"v1", "Pod"}, {"apps/v1", "Deployment"}, {"batch/v1", "Job"},
}

func genObject(t *rapid.T, label string) *unstructured.Unstructured {
	k := rapid.SampledFrom(propKinds).Draw(t, label+"-kind")
	name := rapid.StringMatching(`[a-d]{1,2}`).Draw(t, label+"-name")
	obj := newObj(k.apiVersion, k.kind, rapid.SampledFrom([]string{"", testNS, "other"}).Draw(t, label+"-ns"), name)
	if rapid.Bool().Draw(t, label+"-haswave") {
		wave(obj, rapid.IntRange(-2, 3).Draw(t, label+"-wave"))
	}
	return obj
}

func genHook(t *rapid.T, label string) *unstructured.Unstructured {
	obj := genObject(t, label)
	types := rapid.SliceOfNDistinct(rapid.SampledFrom([]string{"PreSync", "Sync", "PostSync", "SyncFail"}), 1, 3, rapid.ID[string]).Draw(t, label+"-types")
	hookObj(obj, joinComma(types), rapid.SampledFrom([]string{"", "HookSucceeded", "HookFailed", "BeforeHookCreation"}).Draw(t, label+"-policy"))
	if rapid.Bool().Draw(t, label+"-generate") {
		obj.SetGenerateName(obj.GetName() + "-")
		obj.SetName("")
	}
	return obj
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

type genInput struct {
	resources []*unstructured.Unstructured
	hooks     []*unstructured.Unstructured
	live      []*unstructured.Unstructured
	opts      v1alpha1.SyncOptions
}

func drawInput(t *rapid.T) genInput {
	var g genInput
	n := rapid.IntRange(0, 8).Draw(t, "n")
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		obj := genObject(t, fmt.Sprintf("r%d", i))
		k := keyOf(obj).String()
		if seen[k] {
			continue
		}
		seen[k] = true
		g.resources = append(g.resources, obj)
	}
	for i := range rapid.IntRange(0, 3).Draw(t, "hooks") {
		g.hooks = append(g.hooks, genHook(t, fmt.Sprintf("h%d", i)))
	}
	for i := range rapid.IntRange(0, 5).Draw(t, "live") {
		obj := genObject(t, fmt.Sprintf("l%d", i))
		if obj.GetNamespace() == "" {
			obj.SetNamespace(testNS)
		}
		g.live = append(g.live, obj)
	}
	g.opts = rapid.SliceOfDistinct(rapid.SampledFrom([]string{"PruneLast=true", "CreateNamespace=true", "ApplyOutOfSyncOnly=true", "Replace=true"}), rapid.ID[string]).Draw(t, "opts")
	return g
}

func planOf(t *rapid.T, g genInput, resources []*unstructured.Unstructured) []PlannedTask {
	kube := newFakeKube()
	for _, l := range g.live {
		kube.put(l)
	}
	e, err := NewEngine(kube, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	targets := slices.Concat(resources, g.hooks)
	in := &Input{
		OperationUID: "uid",
		App:          AppRef{Name: "app", Destination: v1alpha1.ApplicationDestination{Namespace: testNS}},
		Targets:      targets,
		Live:         kube.live(targets),
		SyncOptions:  g.opts,
		Prune:        true,
		SetTracking:  func(obj *unstructured.Unstructured) error { track(obj); return nil },
	}
	p, err := e.Plan(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return p.Tasks
}

func taskIDs(tasks []PlannedTask) []string {
	var out []string
	for _, t := range tasks {
		out = append(out, fmt.Sprintf("%s/%d/%s/%s", t.Phase, t.Wave, t.Key, t.Action))
	}
	return out
}

// Same inputs give the same plan, independent of the order of the non-hook manifests and of
// map iteration order.
func TestPropertyPlanDeterministic(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		g := drawInput(t)
		first := taskIDs(planOf(t, g, g.resources))
		again := taskIDs(planOf(t, g, g.resources))
		if !slices.Equal(first, again) {
			t.Fatalf("plan not deterministic:\n%v\n%v", first, again)
		}
		perm := rapid.Permutation(g.resources).Draw(t, "perm")
		shuffled := taskIDs(planOf(t, g, perm))
		if !slices.Equal(first, shuffled) {
			t.Fatalf("plan depends on manifest order:\n%v\n%v", first, shuffled)
		}
	})
}

// Without namespace/CRD dependencies the plan is sorted by the task order.
func TestPropertyPlanSorted(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		g := drawInput(t)
		g.opts = slices.DeleteFunc(g.opts, func(s string) bool { return s == "CreateNamespace=true" })
		tasks := planOf(t, g, g.resources)
		for i := 1; i < len(tasks); i++ {
			a, b := tasks[i-1], tasks[i]
			if c := phaseOrder(a.Phase) - phaseOrder(b.Phase); c > 0 || (c == 0 && a.Wave > b.Wave) {
				t.Fatalf("out of order at %d: %v", i, taskIDs(tasks))
			}
			if a.Phase == b.Phase && a.Wave == b.Wave && KindOrder(a.Key.Kind) > KindOrder(b.Key.Kind) {
				t.Fatalf("kind order violated at %d: %v", i, taskIDs(tasks))
			}
		}
	})
}

func genTask(t *rapid.T, label string) *task {
	obj := genObject(t, label)
	obj.SetNamespace(rapid.SampledFrom([]string{"", "a", "b"}).Draw(t, label+"-tns"))
	tk := &task{phase: rapid.SampledFrom([]v1alpha1.SyncPhase{"PreSync", "Sync", "PostSync", "SyncFail"}).Draw(t, label+"-phase")}
	if rapid.Bool().Draw(t, label+"-prune") {
		tk.live = obj
	} else {
		tk.target = obj
	}
	return tk
}

func sortFields(t *task) string {
	o := t.obj()
	return fmt.Sprintf("%s|%d|%s|%s|%s|%s|%v", t.phase, t.wave(), o.GetKind(), o.GroupVersionKind().Group, o.GetNamespace(), o.GetName(), t.isPrune())
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// compareTasks is a total order: antisymmetric, transitive, and equal only for tasks with
// identical sort fields.
func TestPropertyTaskOrderIsTotal(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		a, b, c := genTask(t, "a"), genTask(t, "b"), genTask(t, "c")
		if compareTasks(a, a) != 0 {
			t.Fatal("not reflexive")
		}
		if sign(compareTasks(a, b)) != -sign(compareTasks(b, a)) {
			t.Fatalf("not antisymmetric: %s vs %s", sortFields(a), sortFields(b))
		}
		if (compareTasks(a, b) == 0) != (sortFields(a) == sortFields(b)) {
			t.Fatalf("ties between different tasks: %s vs %s", sortFields(a), sortFields(b))
		}
		if compareTasks(a, b) <= 0 && compareTasks(b, c) <= 0 && compareTasks(a, c) > 0 {
			t.Fatalf("not transitive: %s %s %s", sortFields(a), sortFields(b), sortFields(c))
		}
	})
}

// Hook names are a pure function of their inputs and keep the generateName prefix.
func TestPropertyHookName(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		prefix := rapid.StringMatching(`[a-z]{0,10}-?`).Draw(t, "prefix")
		uid := rapid.String().Draw(t, "uid")
		phase := rapid.SampledFrom([]v1alpha1.SyncPhase{"PreSync", "Sync", "PostSync", "SyncFail"}).Draw(t, "phase")
		w, idx := rapid.Int().Draw(t, "wave"), rapid.IntRange(0, 1000).Draw(t, "idx")
		n := HookName(prefix, uid, phase, w, idx)
		if n != HookName(prefix, uid, phase, w, idx) || len(n) != len(prefix)+8 || n[:len(prefix)] != prefix {
			t.Fatalf("bad hook name %q", n)
		}
	})
}
