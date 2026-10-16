package admin

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"text/tabwriter"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// yamlSeparator separates documents of a YAML stream.
const yamlSeparator = "---\n"

// redactedValue replaces credential values printed to a terminal.
const redactedValue = "<redacted>"

// Output formats of the generate-spec commands.
const (
	formatYAML = "yaml"
	formatJSON = "json"
)

// toMap converts v (a typed object or a map) to its JSON object form.
func toMap(v any) (map[string]any, error) {
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("error marshaling object: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("error unmarshaling object: %w", err)
	}
	return m, nil
}

// specOnly drops status and server-populated metadata from a generated object (upstream
// omitFields keeps the namespace out too; Patrol generators set it only when requested).
func specOnly(m map[string]any) map[string]any {
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		delete(md, "creationTimestamp")
		delete(md, "resourceVersion")
		delete(md, "uid")
		delete(md, "generation")
		delete(md, "managedFields")
	}
	return m
}

// printResources writes objects as YAML (a multi-document stream) or JSON (an object, or a
// v1 List for several objects).
func printResources(w io.Writer, format string, objs ...any) error {
	maps_ := make([]map[string]any, 0, len(objs))
	for _, o := range objs {
		m, err := toMap(o)
		if err != nil {
			return err
		}
		maps_ = append(maps_, specOnly(m))
	}
	switch format {
	case formatYAML:
		for i, m := range maps_ {
			if i > 0 {
				if _, err := io.WriteString(w, yamlSeparator); err != nil {
					return err
				}
			}
			data, err := yaml.Marshal(m)
			if err != nil {
				return fmt.Errorf("error marshaling yaml: %w", err)
			}
			if _, err := w.Write(data); err != nil {
				return err
			}
		}
		return nil
	case formatJSON:
		var v any
		switch len(maps_) {
		case 1:
			v = maps_[0]
		default:
			items := make([]any, len(maps_))
			for i := range maps_ {
				items[i] = maps_[i]
			}
			v = map[string]any{"apiVersion": "v1", "kind": "List", "items": items}
		}
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Errorf("error marshaling json: %w", err)
		}
		_, err = fmt.Fprintln(w, string(data))
		return err
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

// secretForOutput returns a copy of sec in the stringData form generate-spec prints, with
// the values redacted when redact is true.
func secretForOutput(sec *corev1.Secret, redact bool) *corev1.Secret {
	out := sec.DeepCopy()
	out.APIVersion = "v1"
	out.Kind = "Secret"
	out.StringData = map[string]string{}
	for k, v := range sec.Data {
		if redact {
			out.StringData[k] = redactedValue
		} else {
			out.StringData[k] = string(v)
		}
	}
	for k, v := range sec.StringData {
		if redact {
			out.StringData[k] = redactedValue
		} else {
			out.StringData[k] = v
		}
	}
	out.Data = nil
	if len(out.StringData) == 0 {
		out.StringData = nil
	}
	return out
}

// writeFileSecure writes data to path with mode 0600 (also tightening an existing file),
// for outputs that may contain credentials.
func writeFileSecure(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// outputTarget is stdout ("-" or empty) or a file written with mode 0600 on close.
type outputTarget struct {
	path   string
	stdout io.Writer
	w      *bufio.Writer
	file   *os.File
}

// openOutput opens the output named by path ("-" or "" is stdout). Files are created with
// mode 0600 because exports and generated specs may contain credentials.
func openOutput(path string, stdout io.Writer) (*outputTarget, error) {
	t := &outputTarget{path: path, stdout: stdout}
	if path == "" || path == "-" {
		return t, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	t.file = f
	t.w = bufio.NewWriter(f)
	return t, nil
}

func (t *outputTarget) Write(p []byte) (int, error) {
	if t.w != nil {
		return t.w.Write(p)
	}
	return t.stdout.Write(p)
}

// Close flushes and closes a file output.
func (t *outputTarget) Close() error {
	if t.file == nil {
		return nil
	}
	err := t.w.Flush()
	return errors.Join(err, t.file.Close())
}

// newTable returns a tab writer with upstream's table layout.
func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// sortedKeys returns the keys of m, sorted.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// runtimeConvert converts an unstructured object into a typed one.
func runtimeConvert(obj map[string]any, into any) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("error decoding object: %w", err)
	}
	return nil
}
