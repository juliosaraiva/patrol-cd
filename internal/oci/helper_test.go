package oci_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/oci/ocitest"
)

type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	size     int64 // overrides len(body) in the header when non-zero
}

func buildTar(t testing.TB, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typeflag, Linkname: e.linkname}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			hdr.Size = int64(len(e.body))
			if e.size != 0 {
				hdr.Size = e.size
			}
		case tar.TypeDir:
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if hdr.Typeflag == tar.TypeReg && e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("tar body %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

func gz(t testing.TB, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func buildTgz(t testing.TB, entries ...tarEntry) []byte {
	t.Helper()
	return gz(t, buildTar(t, entries...))
}

// newClient returns a client for reg with the given credentials.
func newClient(t testing.TB, reg *ocitest.Registry, opts oci.Options) *oci.Client {
	t.Helper()
	if reg.CACertPEM() != nil && opts.CAData == nil && !opts.InsecureSkipVerify {
		opts.CAData = reg.CACertPEM()
	}
	if reg.CACertPEM() == nil {
		opts.PlainHTTP = true
	}
	c, err := oci.NewClient(opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}
