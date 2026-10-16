package app

import (
	"context"
	"errors"
	"io"

	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// NewAPIClient adapts *apiclient.Client to Client: every method is the client's own except
// the streams, which are wrapped into typed readers.
func NewAPIClient(c *apiclient.Client) Client { return &apiClient{Client: c} }

type apiClient struct {
	*apiclient.Client
}

func (a *apiClient) WatchApplications(ctx context.Context, q *apiclient.ApplicationQuery, opts *apiclient.StreamOptions) (ApplicationEventStream, error) {
	s, err := a.Client.WatchApplications(ctx, q, opts)
	if err != nil {
		return nil, err
	}
	return &typedStream[apiclient.ApplicationWatchEvent]{s: s}, nil
}

func (a *apiClient) WatchApplicationSets(ctx context.Context, req *apiclient.ApplicationSetWatchRequest, opts *apiclient.StreamOptions) (ApplicationSetEventStream, error) {
	s, err := a.Client.WatchApplicationSets(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	return &typedStream[apiclient.ApplicationSetWatchEvent]{s: s}, nil
}

func (a *apiClient) StreamApplicationLogs(ctx context.Context, q *apiclient.ApplicationLogsQuery) (LogStream, error) {
	s, err := a.Client.StreamApplicationLogs(ctx, q)
	if err != nil {
		return nil, err
	}
	return &typedStream[apiclient.LogEntry]{s: s}, nil
}

// typedStream decodes the data of every Server-Sent Event into T.
type typedStream[T any] struct {
	s *apiclient.EventStream
}

func (t *typedStream[T]) Next() (*T, error) {
	ev, err := t.s.Next()
	if err != nil {
		return nil, err
	}
	var out T
	if err := ev.Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (t *typedStream[T]) Close() error { return t.s.Close() }

// isEOF reports whether err ends a stream normally.
func isEOF(err error) bool { return errors.Is(err, io.EOF) }
