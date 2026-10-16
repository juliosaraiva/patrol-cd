package render

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// decodeText converts file content to UTF-8: a UTF-8 byte-order mark is dropped and
// UTF-16 content with a byte-order mark (LE or BE) is transcoded, like upstream
// utfutil in UTF-8 mode. Content without a BOM is returned unchanged.
func decodeText(b []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return b[3:], nil
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return utf16ToUTF8(b[2:], binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return utf16ToUTF8(b[2:], binary.BigEndian)
	}
	return b, nil
}

func utf16ToUTF8(b []byte, order binary.ByteOrder) ([]byte, error) {
	if len(b)%2 != 0 {
		return nil, errors.New("invalid UTF-16 content: odd length")
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = order.Uint16(b[2*i:])
	}
	runes := utf16.Decode(units)
	out := make([]byte, 0, len(runes))
	for _, r := range runes {
		out = utf8.AppendRune(out, r)
	}
	return out, nil
}

// manifestBudget enforces Limits.MaxManifests while output is decoded, so a stream
// of many tiny documents fails once the limit is crossed instead of after the whole
// stream has been turned into objects (each costs far more memory than its text).
// It is shared by every decode of one Generate call (all files and Jsonnet outputs
// of a Directory source). List items count individually, as in postProcess. A nil
// budget or a non-positive max is unlimited.
type manifestBudget struct {
	max int
	n   int
}

func newManifestBudget(max int) *manifestBudget { return &manifestBudget{max: max} }

// take charges u against the budget.
func (b *manifestBudget) take(u *unstructured.Unstructured) error {
	if b == nil || b.max <= 0 || u == nil {
		return nil
	}
	b.n += objectCount(u)
	if b.n > b.max {
		return fmt.Errorf("%w: more than %d objects", ErrTooManyManifests, b.max)
	}
	return nil
}

// objectCount is the number of objects postProcess emits for u: the items of a List
// (any object with an items array, as Unstructured.IsList), none for a null List,
// else one.
func objectCount(u *unstructured.Unstructured) int {
	if u.Object == nil {
		return 0
	}
	if items, ok := u.Object["items"].([]any); ok {
		return len(items)
	}
	if isNullList(u) {
		return 0
	}
	return 1
}

// splitYAMLOrJSON decodes a YAML (multi-document) or JSON stream into objects,
// skipping empty documents, and charges each against budget. On error it returns
// the objects decoded so far.
func splitYAMLOrJSON(r io.Reader, budget *manifestBudget) ([]*unstructured.Unstructured, error) {
	d := kubeyaml.NewYAMLOrJSONDecoder(r, 4096)
	var objs []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if err := d.Decode(&u); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return objs, fmt.Errorf("failed to unmarshal manifest: %w", err)
		}
		if u == nil {
			continue
		}
		if err := budget.take(u); err != nil {
			return objs, err
		}
		objs = append(objs, u)
	}
	return objs, nil
}

// SplitYAML splits tool output (helm, kustomize, plugins) into objects: documents are
// separated by "---", empty and null documents are skipped, and every document must
// be an object with a kind (upstream kube.SplitYAML). It does not limit the number of
// objects; Generate decodes with its Limits.MaxManifests budget.
func SplitYAML(data []byte) ([]*unstructured.Unstructured, error) {
	return splitYAML(data, nil)
}

// splitYAML is SplitYAML charging each object against budget; decoding stops with
// ErrTooManyManifests as soon as the budget is exceeded. On error it returns the
// objects decoded so far.
func splitYAML(data []byte, budget *manifestBudget) ([]*unstructured.Unstructured, error) {
	d := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objs []*unstructured.Unstructured
	for {
		ext := runtime.RawExtension{}
		if err := d.Decode(&ext); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return objs, fmt.Errorf("failed to unmarshal manifest: %w", err)
		}
		raw := bytes.TrimSpace(ext.Raw)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(raw); err != nil {
			return objs, fmt.Errorf("failed to unmarshal manifest: %w", err)
		}
		if err := budget.take(u); err != nil {
			return objs, err
		}
		objs = append(objs, u)
	}
	return objs, nil
}

// decodeJSONArray decodes a JSON array of objects element by element, charging each
// against budget (null elements decode to nil, as json.Unmarshal into a slice of
// pointers). ok is false when data is not an array of objects, mirroring a failed
// json.Unmarshal into []*unstructured.Unstructured; err is non-nil only when the
// budget is exceeded.
func decodeJSONArray(data []byte, budget *manifestBudget) (objs []*unstructured.Unstructured, ok bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, terr := dec.Token()
	if terr != nil || tok != json.Delim('[') {
		return nil, false, nil
	}
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false, nil
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			objs = append(objs, nil)
			continue
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(raw); err != nil {
			return nil, false, nil
		}
		if err := budget.take(u); err != nil {
			return nil, true, err
		}
		objs = append(objs, u)
	}
	if tok, terr := dec.Token(); terr != nil || tok != json.Delim(']') {
		return nil, false, nil
	}
	if _, terr := dec.Token(); !errors.Is(terr, io.EOF) {
		return nil, false, nil
	}
	return objs, true, nil
}

// looksLikeManifest reports whether content contains the three keys of a Kubernetes
// object (upstream heuristic for erroring on malformed manifests instead of skipping
// unrelated YAML).
func looksLikeManifest(content []byte, json bool) bool {
	if json {
		return bytes.Contains(content, []byte(`"apiVersion"`)) &&
			bytes.Contains(content, []byte(`"kind"`)) &&
			bytes.Contains(content, []byte(`"metadata"`))
	}
	return bytes.Contains(content, []byte("apiVersion:")) &&
		bytes.Contains(content, []byte("kind:")) &&
		bytes.Contains(content, []byte("metadata:"))
}
