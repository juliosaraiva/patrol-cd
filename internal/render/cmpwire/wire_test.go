package cmpwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/quick"
)

func checksum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	payload := []byte("archive-bytes")
	m := Metadata{
		AppName:    "app",
		AppRelPath: "apps/app",
		Checksum:   checksum(payload),
		Size:       int64(len(payload)),
		Env:        EnvEntries([]string{"A=1", "B=x=y", "NOEQ", "=empty", "C="}),
	}
	var buf bytes.Buffer
	if err := WriteMetadata(&buf, m); err != nil {
		t.Fatal(err)
	}
	buf.Write(payload)
	got, err := ReadMetadata(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.AppRelPath != m.AppRelPath || got.Checksum != m.Checksum || got.Size != m.Size || len(got.Env) != 3 {
		t.Fatalf("metadata = %+v", got)
	}
	if env := strings.Join(got.Environ(), ","); env != "A=1,B=x=y,C=" {
		t.Fatalf("env = %s", env)
	}
	data, err := io.ReadAll(PayloadReader(&buf, got))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("payload = %q", data)
	}
}

func TestPayloadReaderRejectsTampering(t *testing.T) {
	t.Parallel()
	payload := []byte("0123456789")
	m := Metadata{AppRelPath: ".", Checksum: checksum(payload), Size: int64(len(payload))}
	tests := []struct {
		name string
		body []byte
		want error
	}{
		{"tampered", []byte("0123456780"), ErrChecksumMismatch},
		{"short", payload[:5], ErrSizeMismatch},
		{"trailing data", append(append([]byte{}, payload...), 'x'), ErrSizeMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := io.ReadAll(PayloadReader(bytes.NewReader(tt.body), m))
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
	if ErrChecksumMismatch.Error() != "file checksum validation error" {
		t.Fatalf("checksum error text changed: %q", ErrChecksumMismatch)
	}
}

func TestReadMetadataErrors(t *testing.T) {
	t.Parallel()
	frame := func(n uint32, body string) io.Reader {
		var b bytes.Buffer
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], n)
		b.Write(hdr[:])
		b.WriteString(body)
		return &b
	}
	valid := checksum(nil)
	tests := []struct {
		name string
		r    io.Reader
		want error
	}{
		{"empty stream", bytes.NewReader(nil), ErrMetadataMissing},
		{"zero length", frame(0, ""), ErrMetadataMissing},
		{"too large", frame(MaxMetadataBytes+1, ""), ErrMetadataTooLarge},
		{"not json", frame(3, "abc"), ErrInvalidMetadata},
		{"bad checksum", frame(40, `{"appRelPath":".","checksum":"zz","size":0}`), ErrInvalidMetadata},
		{"escaping app path", frame(uint32(len(`{"appRelPath":"../x","checksum":"`+valid+`","size":0}`)), `{"appRelPath":"../x","checksum":"`+valid+`","size":0}`), ErrInvalidMetadata},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ReadMetadata(tt.r); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
	if ErrMetadataMissing.Error() != "error getting stream metadata: metadata is nil" {
		t.Fatalf("metadata error text changed: %q", ErrMetadataMissing)
	}
}

func TestMetadataValidate(t *testing.T) {
	t.Parallel()
	ok := Metadata{AppRelPath: ".", Checksum: checksum(nil)}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Metadata{
		{AppRelPath: ".", Checksum: checksum(nil), Size: -1},
		{AppRelPath: ".", Checksum: "abc"},
		{AppRelPath: "/abs", Checksum: checksum(nil)},
		{AppRelPath: "a/../../b", Checksum: checksum(nil)},
		{AppRelPath: `a\b`, Checksum: checksum(nil)},
		{AppRelPath: "", Checksum: checksum(nil)},
		{AppRelPath: ".", Checksum: checksum(nil), Env: []EnvEntry{{Name: "A=B", Value: "x"}}},
		{AppRelPath: ".", Checksum: checksum(nil), Env: []EnvEntry{{Name: "", Value: "x"}}},
		{AppRelPath: ".", Checksum: checksum(nil), Env: []EnvEntry{{Name: "A", Value: "x\x00"}}},
	}
	for i, m := range bad {
		if err := m.Validate(); !errors.Is(err, ErrInvalidMetadata) {
			t.Errorf("case %d: error = %v", i, err)
		}
		if _, err := EncodeMetadata(m); err == nil {
			t.Errorf("case %d: EncodeMetadata accepted invalid metadata", i)
		}
	}
	if err := (Metadata{AppRelPath: "a/../../b", Checksum: checksum(nil)}).Validate(); !strings.Contains(err.Error(), "illegal appPath: out of workDir bound") {
		t.Fatalf("message = %v", err)
	}
}

// TestIsLocalRelPathProperty checks that accepted paths never leave their root when
// joined lexically.
func TestIsLocalRelPathProperty(t *testing.T) {
	t.Parallel()
	alphabet := []string{"a", "b", ".", "..", "/", "\\", "x/..", "../"}
	f := func(idx []uint8) bool {
		var b strings.Builder
		for _, i := range idx {
			b.WriteString(alphabet[int(i)%len(alphabet)])
		}
		p := b.String()
		if !IsLocalRelPath(p) {
			return true
		}
		depth := 0
		for _, part := range strings.Split(p, "/") {
			switch part {
			case "", ".":
			case "..":
				depth--
			default:
				depth++
			}
			// path.Clean semantics: a leading ".." can never be cancelled later.
			if depth < 0 {
				return false
			}
		}
		return !strings.HasPrefix(p, "/") && !strings.Contains(p, "\\")
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 5000}); err != nil {
		t.Fatal(err)
	}
}

func TestErrorType(t *testing.T) {
	t.Parallel()
	var err error = &Error{Code: "invalid_argument", Message: "boom"}
	if err.Error() != "boom" {
		t.Fatalf("Error() = %q", err.Error())
	}
}
