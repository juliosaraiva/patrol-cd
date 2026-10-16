package apiclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// DefaultMaxEventBytes bounds one Server-Sent Event (all of its data lines).
const DefaultMaxEventBytes = 16 << 20

// Event is one Server-Sent Event.
type Event struct {
	// ID is the "id:" field (e.g. a resourceVersion to resume from).
	ID string
	// Name is the "event:" field ("" for plain data events).
	Name string
	// Data is the concatenated "data:" lines.
	Data []byte
}

// Decode unmarshals the event data into v.
func (e *Event) Decode(v any) error {
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("decode event: %w", err)
	}
	return nil
}

// EventStream reads Server-Sent Events. Close it when done.
type EventStream struct {
	body    io.ReadCloser
	sc      *bufio.Scanner
	maxSize int
	cancel  context.CancelFunc
	// LastEventID is the ID of the last event received (resume with Last-Event-ID).
	LastEventID string
}

// StreamOptions configures Client.Stream.
type StreamOptions struct {
	// LastEventID is sent as the Last-Event-ID header.
	LastEventID string
	// MaxEventBytes bounds one event (default DefaultMaxEventBytes).
	MaxEventBytes int
}

// Stream opens a Server-Sent Events stream on the route pattern path with the request
// req (see Do). Problems returned before the stream starts are *APIError values; an
// "error" event ends the stream and is returned by Next as *APIError.
func (c *Client) Stream(ctx context.Context, path string, req any, opts *StreamOptions) (*EventStream, error) {
	if opts == nil {
		opts = &StreamOptions{}
	}
	b, err := buildRequest(http.MethodGet, path, req)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	hdr := c.headers.Clone()
	hdr.Set("Accept", "text/event-stream")
	if opts.LastEventID != "" {
		hdr.Set("Last-Event-ID", opts.LastEventID)
	}
	resp, err := c.sendWithHeaders(sctx, b, hdr)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		cancel()
		return nil, errorFromResponse(resp)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("unexpected content type %q for an event stream", resp.Header.Get("Content-Type"))
	}
	maxSize := opts.MaxEventBytes
	if maxSize <= 0 {
		maxSize = DefaultMaxEventBytes
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxSize)
	return &EventStream{body: resp.Body, sc: sc, maxSize: maxSize, cancel: cancel}, nil
}

func (c *Client) sendWithHeaders(ctx context.Context, b *builtRequest, hdr http.Header) (*http.Response, error) {
	u := *c.base
	u.Path = c.base.Path + b.path
	u.RawPath = c.base.Path + b.rawPath
	u.RawQuery = b.query.Encode()
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	hr.Header = hdr
	hr.Header.Set("User-Agent", c.userAgent)
	hr.Header.Set(CSRFHeader, CSRFHeaderValue)
	if tok := c.Token(); tok != "" {
		hr.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.hc.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", b.path, err)
	}
	c.captureRenewal(resp)
	return resp, nil
}

// Next returns the next event. It returns io.EOF when the server ended the stream,
// *APIError for an "error" event, and the context error after cancellation. Heartbeat
// comments are skipped.
func (s *EventStream) Next() (*Event, error) {
	ev := &Event{}
	var data bytes.Buffer
	hasData := false
	for s.sc.Scan() {
		line := s.sc.Text()
		if line == "" {
			if !hasData && ev.Name == "" {
				ev = &Event{}
				continue
			}
			ev.Data = data.Bytes()
			if ev.ID != "" {
				s.LastEventID = ev.ID
			}
			if ev.Name == "error" {
				return nil, eventError(ev)
			}
			return ev, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			if data.Len()+len(value) > s.maxSize {
				return nil, fmt.Errorf("event exceeds %d bytes", s.maxSize)
			}
			data.WriteString(value)
			hasData = true
		case "event":
			ev.Name = value
		case "id":
			if !strings.ContainsRune(value, 0) {
				ev.ID = value
			}
		}
	}
	if err := s.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("event line exceeds %d bytes", s.maxSize)
		}
		return nil, err
	}
	return nil, io.EOF
}

func eventError(ev *Event) error {
	var p Problem
	if err := json.Unmarshal(ev.Data, &p); err != nil || p.Status == 0 {
		return &APIError{StatusCode: 0, Problem: Problem{Title: "stream error", Detail: truncate(string(ev.Data), 512), Code: CodeInternal}}
	}
	return &APIError{StatusCode: p.Status, Problem: p}
}

// Close ends the stream.
func (s *EventStream) Close() error {
	s.cancel()
	return s.body.Close()
}

// Watch reads typed watch events from an open stream until it ends, calling fn for
// each. It returns nil when fn returns ErrStopWatch or the server closed the stream.
func Watch[T any](s *EventStream, fn func(WatchEvent[T]) error) error {
	defer func() { _ = s.Close() }()
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var we WatchEvent[T]
		if err := ev.Decode(&we); err != nil {
			return err
		}
		if err := fn(we); err != nil {
			if errors.Is(err, ErrStopWatch) {
				return nil
			}
			return err
		}
	}
}

// ErrStopWatch stops Watch without an error.
var ErrStopWatch = errors.New("stop watch")
