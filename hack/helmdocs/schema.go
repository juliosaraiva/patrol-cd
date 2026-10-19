package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// jnode is an ordered JSON value: objects keep their key order so that the hand-written parts of
// values.schema.json survive a rewrite unchanged.
type jnode struct {
	obj  *jobject
	arr  []*jnode
	leaf json.RawMessage // scalar
	kind byte            // 'o', 'a' or 's'
}

type jobject struct {
	keys []string
	vals map[string]*jnode
}

func (o *jobject) get(k string) (*jnode, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *jobject) set(k string, v *jnode) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func parseJSON(data []byte) (*jnode, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON document")
	}
	return n, nil
}

func parseValue(dec *json.Decoder) (*jnode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &jobject{vals: map[string]*jnode{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key %v is not a string", kt)
				}
				if _, dup := o.vals[k]; dup {
					return nil, fmt.Errorf("duplicate object key %q", k)
				}
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return &jnode{kind: 'o', obj: o}, nil
		case '[':
			n := &jnode{kind: 'a', arr: []*jnode{}}
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				n.arr = append(n.arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		raw, err := marshalScalar(t)
		if err != nil {
			return nil, err
		}
		return &jnode{kind: 's', leaf: raw}, nil
	}
}

func marshalScalar(v any) (json.RawMessage, error) {
	switch t := v.(type) {
	case json.Number:
		return json.RawMessage(t.String()), nil
	case nil:
		return json.RawMessage("null"), nil
	case bool:
		return json.RawMessage(strconv.FormatBool(t)), nil
	case string:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(t); err != nil {
			return nil, err
		}
		return json.RawMessage(bytes.TrimRight(b.Bytes(), "\n")), nil
	}
	return nil, fmt.Errorf("unsupported scalar %T", v)
}

// encode writes n with two-space indentation; arrays of scalars that fit stay on one line only
// when empty (one element per line otherwise), so diffs of generated enums stay readable.
func (n *jnode) encode(b *bytes.Buffer, indent string) {
	switch n.kind {
	case 's':
		b.Write(n.leaf)
	case 'a':
		if len(n.arr) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, v := range n.arr {
			b.WriteString(indent + "  ")
			v.encode(b, indent+"  ")
			if i < len(n.arr)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "]")
	case 'o':
		if len(n.obj.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range n.obj.keys {
			kb, _ := marshalScalar(k)
			b.WriteString(indent + "  ")
			b.Write(kb)
			b.WriteString(": ")
			n.obj.vals[k].encode(b, indent+"  ")
			if i < len(n.obj.keys)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "}")
	}
}

func str(s string) *jnode {
	raw, _ := marshalScalar(s)
	return &jnode{kind: 's', leaf: raw}
}

func boolean(v bool) *jnode {
	raw, _ := marshalScalar(v)
	return &jnode{kind: 's', leaf: raw}
}

func object(kv ...any) *jnode {
	o := &jobject{vals: map[string]*jnode{}}
	for i := 0; i < len(kv); i += 2 {
		o.set(kv[i].(string), kv[i+1].(*jnode))
	}
	return &jnode{kind: 'o', obj: o}
}

func array(items ...*jnode) *jnode {
	return &jnode{kind: 'a', arr: items}
}

// cmPatterns maps the key patterns of the patrol-cm register (settings.AcceptedKeys) to
// ECMA-262 regular expressions for values.schema.json, with sample keys proving both accept
// the same names. A pattern added to the register without an entry here fails generation.
var cmPatterns = map[string]struct {
	regex   string
	samples []string
}{
	"accounts.<name>":                             {`^accounts\.[^.]+$`, []string{"accounts.alice"}},
	"accounts.<name>.enabled":                     {`^accounts\.[^.]+\.enabled$`, []string{"accounts.alice.enabled"}},
	"help.download.<os>-<arch>":                   {`^help\.download\.(darwin-amd64|darwin-arm64|windows-amd64|linux-amd64|linux-arm64|linux-ppc64le|linux-s390x)$`, []string{"help.download.linux-amd64", "help.download.darwin-arm64"}},
	"resource.customizations.<type>.<group_kind>": {`^resource\.customizations\.(health|useOpenLibs|actions|ignoreDifferences|ignoreResourceUpdates|knownTypeFields)\..+$`, []string{"resource.customizations.health.apps_Deployment", "resource.customizations.ignoreResourceUpdates.all"}},
	"kustomize.path.<version>":                    {`^kustomize\.path\..+$`, []string{"kustomize.path.v5.8.1"}},
	"kustomize.buildOptions.<version>":            {`^kustomize\.buildOptions\..+$`, []string{"kustomize.buildOptions.v5.8.1"}},
	"extension.config.<name>":                     {`^extension\.config\.[^.]+$`, []string{"extension.config.metrics"}},
}

// rejectedCMSamples must be refused by the generated patrol-cm schema (typos and removed keys).
var rejectedCMSamples = []string{"timeout.reconcilation", "repositories", "help.download.plan9-amd64", "resource.customizations.unknown.apps_Deployment"}

// patrolCMSchema builds definitions.patrolCM: exact keys as properties, patterns as
// patternProperties, everything else rejected. Deprecated keys (removed upstream, or without
// effect) are not accepted.
func patrolCMSchema() (*jnode, error) {
	props := object()
	pats := object()
	var exactKeys []string
	var patternKeys []string
	for _, k := range settings.AcceptedKeys() {
		if k.Source != settings.SourceConfigMap || k.Deprecated != "" {
			continue
		}
		if strings.Contains(k.Key, "<") {
			patternKeys = append(patternKeys, k.Key)
			continue
		}
		exactKeys = append(exactKeys, k.Key)
	}
	slices.Sort(exactKeys)
	slices.Sort(patternKeys)
	for _, k := range exactKeys {
		spec, _ := settings.LookupKey(settings.SourceConfigMap, k)
		props.obj.set(k, object("type", str("string"), "description", str(spec.Description)))
	}
	for _, k := range patternKeys {
		p, ok := cmPatterns[k]
		if !ok {
			return nil, fmt.Errorf("patrol-cm key pattern %q has no schema regex in hack/helmdocs (cmPatterns)", k)
		}
		re, err := regexp.Compile(p.regex)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", k, err)
		}
		for _, s := range p.samples {
			spec, ok := settings.LookupKey(settings.SourceConfigMap, s)
			if !ok || spec.Key != k || !re.MatchString(s) {
				return nil, fmt.Errorf("pattern %q: sample %q is not accepted by both the register and %s", k, s, p.regex)
			}
		}
		spec, _ := settings.LookupKey(settings.SourceConfigMap, p.samples[0])
		pats.obj.set(p.regex, object("type", str("string"), "description", str(spec.Description)))
	}
	for k := range cmPatterns {
		if !slices.Contains(patternKeys, k) {
			return nil, fmt.Errorf("cmPatterns entry %q is not a pattern of the patrol-cm register", k)
		}
	}
	for _, s := range rejectedCMSamples {
		if slices.Contains(exactKeys, s) {
			return nil, fmt.Errorf("rejected sample %q is an accepted key", s)
		}
		for _, k := range patternKeys {
			if regexp.MustCompile(cmPatterns[k].regex).MatchString(s) {
				return nil, fmt.Errorf("rejected sample %q matches pattern %q", s, k)
			}
		}
	}
	return object(
		"description", str("patrol-cm keys (generated from internal/settings by hack/helmdocs)"),
		"type", str("object"),
		"properties", props,
		"patternProperties", pats,
		"additionalProperties", boolean(false),
	), nil
}

// patrolRBACCMSchema builds definitions.patrolRBACCM.
func patrolRBACCMSchema() (*jnode, error) {
	props := object()
	var exact []string
	sawPattern := false
	for _, k := range settings.AcceptedKeys() {
		if k.Source != settings.SourceRBACConfigMap || k.Deprecated != "" {
			continue
		}
		if strings.Contains(k.Key, "<") {
			if k.Key != "policy.<name>.csv" {
				return nil, fmt.Errorf("patrol-rbac-cm pattern %q unknown to hack/helmdocs", k.Key)
			}
			sawPattern = true
			continue
		}
		exact = append(exact, k.Key)
	}
	if !sawPattern {
		return nil, errors.New("patrol-rbac-cm register lost the policy.<name>.csv pattern")
	}
	slices.Sort(exact)
	for _, k := range exact {
		spec, _ := settings.LookupKey(settings.SourceRBACConfigMap, k)
		props.obj.set(k, object("type", str("string"), "description", str(spec.Description)))
	}
	overlay, _ := settings.LookupKey(settings.SourceRBACConfigMap, "policy.team-a.csv")
	return object(
		"description", str("patrol-rbac-cm keys (generated from internal/settings by hack/helmdocs)"),
		"type", str("object"),
		"properties", props,
		"patternProperties", object(`^policy\..+\.csv$`, object("type", str("string"), "description", str(overlay.Description))),
		"additionalProperties", boolean(false),
	), nil
}

// cmdParamsKeysSchema builds definitions.cmdParamsKeys (property names of configs.params).
func cmdParamsKeysSchema() *jnode {
	keys := acceptedParamKeys()
	items := make([]*jnode, len(keys))
	for i, k := range keys {
		items[i] = str(k)
	}
	return object(
		"description", str("patrol-cmd-params-cm keys with an effect in Patrol CD (generated from internal/settings by hack/helmdocs)"),
		"enum", array(items...),
	)
}

// renderSchema replaces the generated definitions of values.schema.json.
func renderSchema(in []byte) ([]byte, error) {
	root, err := parseJSON(in)
	if err != nil {
		return nil, err
	}
	if root.kind != 'o' {
		return nil, errors.New("schema root is not an object")
	}
	defs, ok := root.obj.get("definitions")
	if !ok || defs.kind != 'o' {
		return nil, errors.New("schema has no definitions object")
	}
	cm, err := patrolCMSchema()
	if err != nil {
		return nil, err
	}
	rbac, err := patrolRBACCMSchema()
	if err != nil {
		return nil, err
	}
	defs.obj.set("cmdParamsKeys", cmdParamsKeysSchema())
	defs.obj.set("patrolCM", cm)
	defs.obj.set("patrolRBACCM", rbac)
	var b bytes.Buffer
	root.encode(&b, "")
	b.WriteString("\n")
	return b.Bytes(), nil
}
