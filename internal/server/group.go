package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// task is one long-running part of a component (HTTP listener, controller manager, lease
// manager, ...). run must return once ctx ends.
type task struct {
	name string
	run  func(ctx context.Context) error
}

// drainGrace is added to the shutdown timeout of the tasks before the group gives up
// waiting: every task bounds its own drain by the shutdown timeout, the grace covers the
// return path.
const drainGrace = 5 * time.Second

// runGroup runs the tasks until ctx ends or one of them fails, then cancels the others and
// waits at most drain (+ drainGrace) for them to return. A task returning nil before ctx
// ends simply finished. The first failure is returned (with the names of tasks that did not
// stop in time, if any); a clean shutdown returns nil.
func runGroup(ctx context.Context, drain time.Duration, log *slog.Logger, tasks ...task) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(tasks))
	var mu sync.Mutex
	pending := map[string]bool{}
	for _, t := range tasks {
		mu.Lock()
		pending[t.name] = true
		mu.Unlock()
		go func() {
			err := safeRun(ctx, t)
			mu.Lock()
			delete(pending, t.name)
			mu.Unlock()
			results <- result{t.name, err}
		}()
	}
	var errs []error
	remaining := len(tasks)
	// Run phase: wait for a task to end or for ctx to end (shutdown signal or a failure
	// cancelling it). The receive must also watch ctx, so the drain bound below starts at
	// cancellation even when no task returns.
run:
	for remaining > 0 {
		select {
		case r := <-results:
			remaining--
			if r.err != nil && !errors.Is(r.err, context.Canceled) {
				log.Error("component task failed; shutting down", "task", r.name, "error", r.err)
				errs = append(errs, fmt.Errorf("%s: %w", r.name, r.err))
				cancel()
			}
		case <-ctx.Done():
			break run
		}
	}
	cancel()
	deadline := time.NewTimer(drain + drainGrace)
	defer deadline.Stop()
	for remaining > 0 {
		select {
		case r := <-results:
			remaining--
			if r.err != nil && !errors.Is(r.err, context.Canceled) {
				errs = append(errs, fmt.Errorf("%s: %w", r.name, r.err))
			}
		case <-deadline.C:
			mu.Lock()
			names := make([]string, 0, len(pending))
			for n := range pending {
				names = append(names, n)
			}
			mu.Unlock()
			slices.Sort(names)
			errs = append(errs, fmt.Errorf("shutdown did not complete within %s: still running: %v", drain+drainGrace, names))
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}

// safeRun runs a task, converting a panic into an error so one failing part shuts the
// component down cleanly instead of crashing without draining the others.
func safeRun(ctx context.Context, t task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return t.run(ctx)
}
