package apiserver

import (
	"errors"
	"net"
	"testing"
)

// TestSplitListenerCloseIdempotent: the TLS and redirect servers each close their
// sub-listener on Shutdown; only the first close may touch (and report on) the shared
// socket.
func TestSplitListenerCloseIdempotent(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := newSplitListener(l)
	tlsSide, plainSide := subListener{s, s.tlsCh}, subListener{s, s.plainCh}
	if err := tlsSide.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := plainSide.Close(); err != nil {
		t.Fatalf("second close of the shared listener must be a no-op, got %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("third close: %v", err)
	}
	for _, sub := range []subListener{tlsSide, plainSide} {
		if _, err := sub.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after close: got %v, want net.ErrClosed", err)
		}
	}
}
