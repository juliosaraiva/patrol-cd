package controller

import (
	"container/list"
	"context"
	"encoding/json"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// DefaultOperationCacheBytes bounds the memory used to keep terminal Operations.
const DefaultOperationCacheBytes = 32 << 20

func newUnstructured(kind string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(kind))
	return u
}

func newApplication() *unstructured.Unstructured { return newUnstructured("Application") }
func newAppProject() *unstructured.Unstructured  { return newUnstructured("AppProject") }
func newOperation() *unstructured.Unstructured   { return newUnstructured("Operation") }

// NewOperationGetter returns an OperationGetter that reads Operations through reader (the
// controller passes an uncached API reader, so Operations are never watched) and keeps the
// fields notifications use of terminal Operations, which no longer change, in an LRU bounded to
// maxBytes of JSON (DefaultOperationCacheBytes when <= 0).
func NewOperationGetter(reader client.Reader, maxBytes int64) OperationGetter {
	if maxBytes <= 0 {
		maxBytes = DefaultOperationCacheBytes
	}
	return &operationGetter{reader: reader, cache: newWeightedLRU(maxBytes)}
}

type operationGetter struct {
	reader client.Reader
	cache  *weightedLRU
}

func (g *operationGetter) GetOperation(ctx context.Context, namespace, name string) (map[string]any, error) {
	key := namespace + "/" + name
	if v, ok := g.cache.get(key); ok {
		return v, nil
	}
	u := newOperation()
	if err := g.reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, u); err != nil {
		return nil, err
	}
	subset := operationSubset(u.Object)
	status, _ := subset["status"].(map[string]any)
	phase, _ := status["phase"].(string)
	if terminalPhase(phase) {
		if b, err := json.Marshal(subset); err == nil {
			g.cache.add(key, subset, int64(len(b)))
		}
	}
	return subset, nil
}

// operationSubset keeps the Operation fields TemplateApplication uses.
func operationSubset(obj map[string]any) map[string]any {
	out := map[string]any{}
	if spec, ok := obj["spec"].(map[string]any); ok {
		s := map[string]any{}
		for _, k := range []string{"application", "rollback", "initiatedBy", "info", "retry"} {
			if v, ok := spec[k]; ok {
				s[k] = copyJSON(v)
			}
		}
		if sync, ok := spec["sync"].(map[string]any); ok {
			c, _ := copyJSON(sync).(map[string]any)
			delete(c, "manifests")
			s["sync"] = c
		}
		out["spec"] = s
	}
	if status, ok := obj["status"].(map[string]any); ok {
		s := map[string]any{}
		for _, k := range []string{"phase", "message", "startedAt", "finishedAt", "retryCount", "syncResult"} {
			if v, ok := status[k]; ok {
				s[k] = copyJSON(v)
			}
		}
		out["status"] = s
	}
	return out
}

// weightedLRU is a concurrency-safe LRU bounded by the summed weight of its values. Values larger
// than an eighth of the budget are not kept.
type weightedLRU struct {
	mu      sync.Mutex
	max     int64
	used    int64
	order   *list.List
	entries map[string]*list.Element
}

type lruEntry struct {
	key    string
	value  map[string]any
	weight int64
}

func newWeightedLRU(maxBytes int64) *weightedLRU {
	return &weightedLRU{max: maxBytes, order: list.New(), entries: map[string]*list.Element{}}
}

func (c *weightedLRU) get(key string) (map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry).value, true
}

func (c *weightedLRU) add(key string, value map[string]any, weight int64) {
	if weight <= 0 {
		weight = 1
	}
	if weight > c.max/8 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.used -= el.Value.(*lruEntry).weight
		c.order.Remove(el)
		delete(c.entries, key)
	}
	c.entries[key] = c.order.PushFront(&lruEntry{key: key, value: value, weight: weight})
	c.used += weight
	for c.used > c.max {
		last := c.order.Back()
		if last == nil {
			break
		}
		e := last.Value.(*lruEntry)
		c.order.Remove(last)
		delete(c.entries, e.key)
		c.used -= e.weight
	}
}

func (c *weightedLRU) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
