package main

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
)

func tarOf(t *testing.T, hs ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range hs {
		if h.Typeflag == tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

const expectedFile = `
# comment
d 0755 999:999 /home/patrol
x 0755 0:0 /usr/local/bin/patrolcd
f 0644 0:0 /etc/gitconfig
l - 0:0 /usr/local/bin/patrol-server patrolcd
absent - - /sbin/apk
`

func goodTar(t *testing.T) *bytes.Buffer {
	return tarOf(t,
		&tar.Header{Name: "home/patrol/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 999, Gid: 999},
		&tar.Header{Name: "usr/local/bin/patrolcd", Typeflag: tar.TypeReg, Mode: 0o755},
		&tar.Header{Name: "etc/gitconfig", Typeflag: tar.TypeReg, Mode: 0o644},
		&tar.Header{Name: "usr/local/bin/patrol-server", Typeflag: tar.TypeSymlink, Linkname: "patrolcd", Mode: 0o777},
	)
}

func TestCheck(t *testing.T) {
	exps, err := parseExpected(strings.NewReader(expectedFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(exps) != 5 {
		t.Fatalf("parsed %d expectations, want 5", len(exps))
	}
	entries, err := readTar(goodTar(t))
	if err != nil {
		t.Fatal(err)
	}
	if fails := check(exps, entries); len(fails) != 0 {
		t.Fatalf("matching image reported failures: %v", fails)
	}

	cases := map[string]struct {
		mutate func(map[string]entry)
		want   string
	}{
		"missing path": {func(m map[string]entry) { delete(m, "/etc/gitconfig") }, "missing"},
		"wrong mode":   {func(m map[string]entry) { e := m["/home/patrol"]; e.mode = 0o700; m["/home/patrol"] = e }, "mode 0700, want 0755"},
		"wrong owner": {func(m map[string]entry) {
			e := m["/usr/local/bin/patrolcd"]
			e.uid = 1
			m["/usr/local/bin/patrolcd"] = e
		}, "owner 1:0, want 0:0"},
		"wrong link": {func(m map[string]entry) {
			e := m["/usr/local/bin/patrol-server"]
			e.link = "other"
			m["/usr/local/bin/patrol-server"] = e
		}, `link target "other"`},
		"wrong type":      {func(m map[string]entry) { e := m["/etc/gitconfig"]; e.typeflag = tar.TypeDir; m["/etc/gitconfig"] = e }, "want \"f\""},
		"present, absent": {func(m map[string]entry) { m["/sbin/apk"] = entry{typeflag: tar.TypeReg} }, "must be absent"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := readTar(goodTar(t))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(m)
			fails := check(exps, m)
			if len(fails) != 1 || !strings.Contains(fails[0], tc.want) {
				t.Fatalf("failures = %v, want one containing %q", fails, tc.want)
			}
		})
	}
}

func TestParseExpectedErrors(t *testing.T) {
	for _, in := range []string{"d 0755 0:0", "q 0755 0:0 /x", "l - 0:0 /x", "d 9 0:0 /x", "d 0755 0:0 rel", "d 0755 0:0 /x extra"} {
		if _, err := parseExpected(strings.NewReader(in)); err == nil {
			t.Errorf("parseExpected(%q) succeeded, want an error", in)
		}
	}
}
