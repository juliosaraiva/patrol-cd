package transport

import (
	"errors"
	"testing"

	"k8s.io/client-go/rest"
)

func TestPodDialerArgumentsAndClose(t *testing.T) {
	t.Parallel()
	d, err := NewPodDialer(&rest.Config{Host: "https://127.0.0.1:1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := d.DialPod(t.Context(), "patrol", "p", port); err == nil {
			t.Fatalf("port %d accepted", port)
		}
	}
	if _, err := d.DialPod(t.Context(), "patrol", "p", 8081); err == nil {
		t.Fatal("dial to an unreachable API server succeeded")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal("second Close:", err)
	}
	if _, err := d.DialPod(t.Context(), "patrol", "p", 8081); !errors.Is(err, ErrPodDialerClosed) {
		t.Fatalf("dial after Close: %v", err)
	}
}
