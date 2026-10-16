package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// maxDocuments bounds the documents read from one input.
const maxDocuments = 10000

// splitYAMLDocuments splits YAML or JSON input separated by "---" into JSON documents,
// skipping empty and null documents.
func splitYAMLDocuments(data []byte) ([][]byte, error) {
	d := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out [][]byte
	for {
		var ext runtime.RawExtension
		if err := d.Decode(&ext); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("failed to parse manifest: %w", err)
		}
		raw := bytes.TrimSpace(ext.Raw)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		if len(out) >= maxDocuments {
			return nil, fmt.Errorf("input has more than %d documents", maxDocuments)
		}
		out = append(out, raw)
	}
}

// decodeStrict decodes one JSON document into out, rejecting unknown fields.
func decodeStrict(doc []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the document")
	}
	return nil
}

// manifestsToObjects parses JSON (or YAML) manifests into objects.
func manifestsToObjects(manifests []string) ([]*unstructured.Unstructured, error) {
	out := make([]*unstructured.Unstructured, 0, len(manifests))
	for _, m := range manifests {
		obj, err := unmarshalObject(m)
		if err != nil {
			return nil, err
		}
		if obj != nil {
			out = append(out, obj)
		}
	}
	return out, nil
}

// unmarshalObject parses one manifest; "", "null" give nil.
func unmarshalObject(manifest string) (*unstructured.Unstructured, error) {
	s := bytes.TrimSpace([]byte(manifest))
	if len(s) == 0 || bytes.Equal(s, []byte("null")) {
		return nil, nil
	}
	docs, err := splitYAMLDocuments(s)
	if err != nil {
		return nil, err
	}
	if len(docs) != 1 {
		return nil, fmt.Errorf("expected one object, got %d documents", len(docs))
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(docs[0]); err != nil {
		return nil, fmt.Errorf("failed to unmarshal manifest: %w", err)
	}
	return u, nil
}
