package cmp

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/runtime"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// splitManifests splits a YAML or JSON object stream into documents, each returned as
// compact JSON. Empty and null documents are skipped (upstream SplitYAMLToString).
func splitManifests(data []byte) ([]string, error) {
	d := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	out := []string{}
	for {
		var ext runtime.RawExtension
		if err := d.Decode(&ext); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("failed to unmarshal manifest: %w", err)
		}
		raw := bytes.TrimSpace(ext.Raw)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		out = append(out, string(raw))
	}
}
