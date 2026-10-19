package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// The checks below parse YAML the way kubectl does (sigs.k8s.io/yaml) and remove the Helm
// metadata with their own code, independently of the yaml.v3 node handling that produced the
// files, so a defect in either path shows up as a difference.

// genericObject is a decoded object in kubectl's representation.
type genericObject struct {
	id string
	m  map[string]any
}

func genericObjects(data []byte) ([]genericObject, error) {
	rd := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var out []genericObject
	for {
		doc, err := rd.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := yaml.Unmarshal(doc, &m); err != nil {
			return nil, err
		}
		if len(m) == 0 {
			continue
		}
		meta, _ := m["metadata"].(map[string]any)
		ns, _ := meta["namespace"].(string)
		name, _ := meta["name"].(string)
		out = append(out, genericObject{id: fmt.Sprintf("%v/%v/%s/%s", m["apiVersion"], m["kind"], ns, name), m: m})
	}
}

// stripHelmGeneric removes helm.sh/chart, app.kubernetes.io/managed-by and helm.sh/* /
// meta.helm.sh/* annotations from every metadata map, dropping maps left empty.
func stripHelmGeneric(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if meta, ok := child.(map[string]any); ok && k == "metadata" {
				if labels, ok := meta["labels"].(map[string]any); ok {
					delete(labels, "helm.sh/chart")
					delete(labels, "app.kubernetes.io/managed-by")
					if len(labels) == 0 {
						delete(meta, "labels")
					}
				}
				if ann, ok := meta["annotations"].(map[string]any); ok {
					for a := range ann {
						if strings.HasPrefix(a, "helm.sh/") || strings.HasPrefix(a, "meta.helm.sh/") {
							delete(ann, a)
						}
					}
					if len(ann) == 0 {
						delete(meta, "annotations")
					}
				}
			}
			stripHelmGeneric(child)
		}
	case []any:
		for _, c := range t {
			stripHelmGeneric(c)
		}
	}
}

// expectedNamespace is the namespace object of the cluster-wide variants.
func expectedNamespace(appVersion string) genericObject {
	return genericObject{
		id: "v1/Namespace//" + namespace,
		m: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{
				"name": namespace,
				"labels": map[string]any{
					"app.kubernetes.io/instance":                 releaseName,
					"app.kubernetes.io/part-of":                  "patrol-cd",
					"app.kubernetes.io/version":                  appVersion,
					"pod-security.kubernetes.io/enforce":         "restricted",
					"pod-security.kubernetes.io/enforce-version": psaEnforceVersion,
					"pod-security.kubernetes.io/audit":           "restricted",
					"pod-security.kubernetes.io/audit-version":   "latest",
					"pod-security.kubernetes.io/warn":            "restricted",
					"pod-security.kubernetes.io/warn-version":    "latest",
				},
			},
		},
	}
}

// verifyVariant checks that a built manifest holds exactly the chart render's objects without
// the Helm metadata (plus the namespace of a cluster-wide variant), in kind order, with the
// namespace first.
func verifyVariant(v variant, built, raw []byte, appVersion string) error {
	want, err := genericObjects(raw)
	if err != nil {
		return fmt.Errorf("chart render: %w", err)
	}
	for _, o := range want {
		stripHelmGeneric(o.m)
	}
	if v.cluster {
		want = append(want, expectedNamespace(appVersion))
	}
	got, err := genericObjects(built)
	if err != nil {
		return fmt.Errorf("built manifest: %w", err)
	}
	if err := sameObjects(got, want); err != nil {
		return err
	}
	if v.cluster && (len(got) == 0 || got[0].m["kind"] != "Namespace") {
		return errors.New("the namespace is not the first object")
	}
	return checkKindOrder(got)
}

// verifyCRDs checks the CRD manifest: exactly the render's CRDs and admission objects.
func verifyCRDs(built, raw []byte) error {
	all, err := genericObjects(raw)
	if err != nil {
		return fmt.Errorf("chart render: %w", err)
	}
	var want []genericObject
	for _, o := range all {
		if k, _ := o.m["kind"].(string); slices.Contains(crdKinds, k) {
			stripHelmGeneric(o.m)
			want = append(want, o)
		}
	}
	got, err := genericObjects(built)
	if err != nil {
		return fmt.Errorf("built manifest: %w", err)
	}
	if err := sameObjects(got, want); err != nil {
		return err
	}
	return checkKindOrder(got)
}

// sameObjects compares two object sets (order-insensitive) by their JSON encoding.
func sameObjects(got, want []genericObject) error {
	index := func(objs []genericObject) (map[string][]byte, error) {
		m := map[string][]byte{}
		for _, o := range objs {
			if _, dup := m[o.id]; dup {
				return nil, fmt.Errorf("duplicate object %s", o.id)
			}
			b, err := json.Marshal(o.m)
			if err != nil {
				return nil, err
			}
			m[o.id] = b
		}
		return m, nil
	}
	g, err := index(got)
	if err != nil {
		return err
	}
	w, err := index(want)
	if err != nil {
		return err
	}
	var problems []string
	for id, wb := range w {
		gb, ok := g[id]
		switch {
		case !ok:
			problems = append(problems, "missing "+id)
		case !bytes.Equal(gb, wb):
			problems = append(problems, "differs from the chart render: "+id)
		}
	}
	for id := range g {
		if _, ok := w[id]; !ok {
			problems = append(problems, "not in the chart render: "+id)
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("manifest and chart render differ:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// checkKindOrder requires the objects to follow kindOrder.
func checkKindOrder(objs []genericObject) error {
	for i := 1; i < len(objs); i++ {
		a, _ := objs[i-1].m["kind"].(string)
		b, _ := objs[i].m["kind"].(string)
		if kindRank(b) < kindRank(a) {
			return fmt.Errorf("apply order: %s before %s", objs[i-1].id, objs[i].id)
		}
	}
	return nil
}
