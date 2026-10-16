package sync

import (
	"context"
	"fmt"
	"slices"
	"strings"
	gosync "sync"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// resultStore holds one result row per (resource, sync phase) in first-recorded order.
// Tasks executed concurrently reserve their position beforehand, so the order is the task
// order and does not depend on goroutine scheduling. It is seeded from the persisted results
// and is safe for concurrent use.
type resultStore struct {
	mu    gosync.Mutex
	index map[string]int
	rows  []v1alpha1.ResourceResult
	order []int
	next  int
}

func newResultStore(prev []v1alpha1.ResourceResult) *resultStore {
	s := &resultStore{index: make(map[string]int, len(prev))}
	for _, r := range prev {
		k := rowKey(r)
		if i, ok := s.index[k]; ok {
			s.rows[i] = r // duplicate rows: keep the last
			continue
		}
		s.index[k] = len(s.rows)
		s.rows = append(s.rows, r)
		s.next++
		s.order = append(s.order, s.next)
	}
	return s
}

// reserve assigns t the next result position unless it already has a row or a position.
func (s *resultStore) reserve(t *task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.order != 0 {
		return
	}
	if _, ok := s.index[t.resultKey()]; ok {
		return
	}
	s.next++
	t.order = s.next
}

func (s *resultStore) get(key string) (v1alpha1.ResourceResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.index[key]
	if !ok {
		return v1alpha1.ResourceResult{}, false
	}
	return s.rows[i], true
}

func (s *resultStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

// set records the task's current state; the latest non-empty message is kept.
func (s *resultStore) set(t *task, status v1alpha1.ResultCode, phase v1alpha1.OperationPhase, message string) {
	t.status = status
	t.opState = phase
	if message != "" {
		t.message = message
	}
	k := t.key()
	row := v1alpha1.ResourceResult{
		Group:     k.Group,
		Version:   t.version(),
		Kind:      k.Kind,
		Namespace: k.Namespace,
		Name:      k.Name,
		Status:    t.status,
		Message:   t.message,
		HookType:  t.hookType(),
		HookPhase: t.opState,
		SyncPhase: t.phase,
		Images:    resourceImages(t.obj()),
	}
	rk := t.resultKey()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.index[rk]; ok {
		existing := &s.rows[i]
		existing.Status = row.Status
		existing.HookPhase = row.HookPhase
		existing.Message = row.Message
		return
	}
	ord := t.order
	if ord == 0 {
		s.next++
		ord = s.next
	}
	s.index[rk] = len(s.rows)
	s.rows = append(s.rows, row)
	s.order = append(s.order, ord)
}

// snapshot returns a copy of the rows in recording order.
func (s *resultStore) snapshot() []v1alpha1.ResourceResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := make([]int, len(s.rows))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return s.order[a] - s.order[b] })
	out := make([]v1alpha1.ResourceResult, len(s.rows))
	for i, j := range idx {
		r := s.rows[j]
		r.Images = slices.Clone(r.Images)
		out[i] = r
	}
	return out
}

// failedMessages returns the sorted distinct non-empty messages of SyncFailed rows.
func (s *resultStore) failedMessages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var msgs []string
	for _, r := range s.rows {
		if r.Status == v1alpha1.ResultCodeSyncFailed && r.Message != "" && !seen[r.Message] {
			seen[r.Message] = true
			msgs = append(msgs, r.Message)
		}
	}
	slices.Sort(msgs)
	return msgs
}

// runningMessage builds the Running message of a step that waits on tasks, mirroring
// upstream setRunningPhase: hooks are named first; pending prune confirmations take
// precedence when confirmation is missing.
func runningMessage(tasks syncTasks, pendingDeletion bool, pruneConfirmed bool, opts Options) string {
	if len(tasks) == 0 {
		return ""
	}
	hooks, resources := tasks.split(func(t *task) bool { return t.hook })
	reason := "completion of hook"
	taskType := "resources"
	if len(hooks) == 0 {
		reason = "healthy state of"
	}
	if len(resources) == 0 {
		taskType = "hooks"
	}
	if pendingDeletion {
		reason = "deletion of"
		if len(hooks) != 0 {
			reason += " hook"
		}
	}
	first := tasks[0]
	if len(hooks) != 0 {
		first = hooks[0]
	}
	more := len(tasks) - 1
	if !pruneConfirmed {
		toConfirm := tasks.filter(func(t *task) bool { return t.isPrune() && opts.RequiresPruneConfirmation(t.live) })
		if len(toConfirm) > 0 {
			reason = "pruning confirmation of"
			first = toConfirm[0]
			more = len(toConfirm) - 1
		}
	}
	msg := fmt.Sprintf("waiting for %s %s/%s/%s", reason, first.group(), first.kind(), first.name())
	if more > 0 {
		msg = fmt.Sprintf("%s and %d more %s", msg, more, taskType)
	}
	return msg
}

const (
	msgServerCouldNotFind = "the server could not find the requested resource"
	msgAnnotationTooLong  = "metadata.annotations: Too long: must have at most 262144 bytes"
	annotationTooLongHint = " \n -Additional Info: This error usually means that you are trying to add a large resource on client side. Consider using Server-side apply or syncing with replace enabled. Note: Syncing with Replace enabled is potentially destructive as it may cause resource deletion and re-creation."
)

// augmentMessages rewrites result messages like upstream AugmentSyncMsg (SYNC-072):
// missing-kind errors name the CRD to install (or the installed version), and annotation
// size errors get a hint to use server-side apply or replace.
func augmentMessages(ctx context.Context, kube Kube, rows []v1alpha1.ResourceResult) {
	for i := range rows {
		r := &rows[i]
		switch {
		case strings.Contains(r.Message, msgServerCouldNotFind):
			if kube == nil {
				continue
			}
			res, err := kube.Resource(ctx, schema.GroupVersionKind{Group: r.Group, Kind: r.Kind})
			if err != nil || res == nil {
				r.Message = fmt.Sprintf("The Kubernetes API could not find %s/%s for requested resource %s/%s. Make sure the %q CRD is installed on the destination cluster.",
					r.Group, r.Kind, r.Namespace, r.Name, r.Kind)
				continue
			}
			r.Message = fmt.Sprintf("The Kubernetes API could not find version %q of %s/%s for requested resource %s/%s. Version %q of %s/%s is installed on the destination cluster.",
				r.Version, r.Group, r.Kind, r.Namespace, r.Name, res.Version, r.Group, r.Kind)
		case strings.Contains(r.Message, msgAnnotationTooLong) && !strings.Contains(r.Message, annotationTooLongHint):
			r.Message += annotationTooLongHint
		}
	}
}
