package cache

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type objOpt func(*unstructured.Unstructured)

func withOwner(apiVersion, kind, name, uid string) objOpt {
	return func(u *unstructured.Unstructured) {
		refs := u.GetOwnerReferences()
		refs = append(refs, metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: types.UID(uid)})
		u.SetOwnerReferences(refs)
	}
}

func withLabels(l map[string]string) objOpt {
	return func(u *unstructured.Unstructured) { u.SetLabels(l) }
}

func withAnnotations(a map[string]string) objOpt {
	return func(u *unstructured.Unstructured) { u.SetAnnotations(a) }
}

func withRV(rv string) objOpt {
	return func(u *unstructured.Unstructured) { u.SetResourceVersion(rv) }
}

func withField(value any, path ...string) objOpt {
	return func(u *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(u.Object, value, path...); err != nil {
			panic(err) // test helper only
		}
	}
}

func mkObj(apiVersion, kind, ns, name, uid string, opts ...objOpt) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetUID(types.UID(uid))
	u.SetResourceVersion("1")
	for _, o := range opts {
		o(u)
	}
	return u
}

func mkRes(apiVersion, kind, ns, name, uid string, opts ...objOpt) *Resource {
	return newResource(mkObj(apiVersion, kind, ns, name, uid, opts...), &Settings{})
}

// eventually polls cond until it returns true or timeout elapses.
func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s: "+msg, append([]any{timeout}, args...)...)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
