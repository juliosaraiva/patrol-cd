// Package core holds what the Application and Operation reconcilers share: their
// dependencies (Deps), controller parameters (Params), the Applications-in-any-namespace
// matcher, Operation listing and ordering, optimistic-concurrency status writes, revision
// history bookkeeping, refresh levels, Event reasons and Prometheus metrics.
package core
