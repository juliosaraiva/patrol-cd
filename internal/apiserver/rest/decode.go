package rest

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// paramField is a struct field bound to a path or query parameter.
type paramField struct {
	name   string
	goName string
	index  []int
	typ    reflect.Type
	// multi is true for slice fields (query parameters that may repeat).
	multi bool
}

// typeMeta describes how a request type binds to an HTTP request.
type typeMeta struct {
	typ   reflect.Type
	path  []paramField
	query []paramField
	// bodyIndex is the index of the `body:"true"` field, nil when the whole struct is the
	// body.
	bodyIndex []int
	// bodyType is the type decoded from the JSON body.
	bodyType reflect.Type
	// hasBody is true when a JSON body can carry data.
	hasBody bool
	// k8sObject is true when the body is a Kubernetes object (unknown fields accepted).
	k8sObject bool
}

// QueryParam describes a query parameter of a request type (for documentation).
type QueryParam struct {
	Name  string
	Type  reflect.Type
	Multi bool
}

// RequestBinding describes how a request type binds to HTTP (for documentation).
type RequestBinding struct {
	PathParams  map[string]reflect.Type
	QueryParams []QueryParam
	// BodyType is the JSON body type, or nil when the request has no body.
	BodyType reflect.Type
	// BodyIsStruct is true when the body is the request type itself (path and query
	// fields excluded), false when it is a `body:"true"` field.
	BodyIsStruct bool
}

// BindingOf returns the binding of a request type.
func BindingOf(t reflect.Type) (RequestBinding, error) {
	m, err := metaFor(t)
	if err != nil {
		return RequestBinding{}, err
	}
	b := RequestBinding{PathParams: map[string]reflect.Type{}}
	for _, f := range m.path {
		b.PathParams[f.name] = f.typ
	}
	for _, f := range m.query {
		b.QueryParams = append(b.QueryParams, QueryParam{Name: f.name, Type: f.typ, Multi: f.multi})
	}
	if m.hasBody {
		b.BodyType = m.bodyType
		b.BodyIsStruct = m.bodyIndex == nil
	}
	return b, nil
}

var metaCache sync.Map // reflect.Type -> *typeMeta or error

var (
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	runtimeObjectType   = reflect.TypeFor[runtime.Object]()
)

func metaFor(t reflect.Type) (*typeMeta, error) {
	if v, ok := metaCache.Load(t); ok {
		if err, isErr := v.(error); isErr {
			return nil, err
		}
		return v.(*typeMeta), nil
	}
	m, err := buildMeta(t)
	if err != nil {
		metaCache.Store(t, err)
		return nil, err
	}
	metaCache.Store(t, m)
	return m, nil
}

func buildMeta(t reflect.Type) (*typeMeta, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("request type %s must be a struct", t)
	}
	m := &typeMeta{typ: t, bodyType: t}
	jsonVisible := 0
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() || f.Anonymous {
			continue
		}
		if len(f.Index) > 1 && hasPointerOnPath(t, f.Index) {
			if f.Tag.Get("path") != "" || f.Tag.Get("query") != "" {
				return nil, fmt.Errorf("%s.%s: path/query fields cannot be promoted through embedded pointers", t, f.Name)
			}
		}
		pathName, queryName := f.Tag.Get("path"), f.Tag.Get("query")
		isBody := f.Tag.Get("body") == "true"
		jsonTag := f.Tag.Get("json")
		jsonName, _, _ := strings.Cut(jsonTag, ",")
		switch {
		case pathName != "" && queryName != "":
			return nil, fmt.Errorf("%s.%s: field cannot be both a path and a query parameter", t, f.Name)
		case pathName != "" || queryName != "":
			if jsonName != "-" {
				return nil, fmt.Errorf("%s.%s: path and query fields must be tagged json:\"-\"", t, f.Name)
			}
			multi := f.Type.Kind() == reflect.Slice
			if pathName != "" && multi {
				return nil, fmt.Errorf("%s.%s: path parameters cannot be slices", t, f.Name)
			}
			elem := f.Type
			if multi {
				elem = elem.Elem()
			}
			if !scalarSupported(elem) {
				return nil, fmt.Errorf("%s.%s: unsupported parameter type %s", t, f.Name, f.Type)
			}
			pf := paramField{name: pathName, goName: f.Name, index: f.Index, typ: f.Type, multi: multi}
			if pathName != "" {
				m.path = append(m.path, pf)
			} else {
				pf.name = queryName
				m.query = append(m.query, pf)
			}
		case isBody:
			if m.bodyIndex != nil {
				return nil, fmt.Errorf("%s: more than one body field", t)
			}
			if jsonName != "-" {
				return nil, fmt.Errorf("%s.%s: the body field must be tagged json:\"-\"", t, f.Name)
			}
			m.bodyIndex = f.Index
			m.bodyType = f.Type
		default:
			if jsonName != "-" {
				jsonVisible++
			}
		}
	}
	if m.bodyIndex != nil {
		if jsonVisible > 0 {
			return nil, fmt.Errorf("%s: a body field excludes other JSON fields", t)
		}
		m.hasBody = true
	} else {
		m.hasBody = jsonVisible > 0
	}
	bt := m.bodyType
	for bt.Kind() == reflect.Pointer {
		bt = bt.Elem()
	}
	m.k8sObject = reflect.PointerTo(bt).Implements(runtimeObjectType)
	return m, nil
}

func hasPointerOnPath(t reflect.Type, index []int) bool {
	for _, i := range index[:len(index)-1] {
		f := t.Field(i)
		if f.Type.Kind() == reflect.Pointer {
			return true
		}
		t = f.Type
	}
	return false
}

func scalarSupported(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(textUnmarshalerType) {
		return true
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// setScalar parses s into v (a settable value of a supported scalar type, or a pointer
// to one).
func setScalar(v reflect.Value, s string) error {
	if v.Kind() == reflect.Pointer {
		p := reflect.New(v.Type().Elem())
		if err := setScalar(p.Elem(), s); err != nil {
			return err
		}
		v.Set(p)
		return nil
	}
	if tu, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
		return tu.UnmarshalText([]byte(s))
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		if s == "" {
			v.SetBool(true)
			return nil
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return errors.New("not a boolean")
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return errors.New("not an integer in range")
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return errors.New("not an unsigned integer in range")
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return errors.New("not a number")
		}
		v.SetFloat(n)
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
	return nil
}

type decodeOptions struct {
	maxBody      int64
	allowUnknown bool
}

// maxQueryValues bounds the number of values accepted for one repeated query parameter.
const maxQueryValues = 1000

func decodeRequest(w http.ResponseWriter, r *http.Request, m *typeMeta, dst reflect.Value, opts decodeOptions) error {
	for _, f := range m.path {
		raw := r.PathValue(f.name)
		if err := setScalar(dst.FieldByIndex(f.index), raw); err != nil {
			return InvalidArgument("invalid path parameter %q: %v", f.name, err)
		}
	}
	if len(m.query) > 0 {
		q, err := parseQuery(r)
		if err != nil {
			return err
		}
		for _, f := range m.query {
			vals, ok := q[f.name]
			if !ok {
				continue
			}
			fv := dst.FieldByIndex(f.index)
			if f.multi {
				if len(vals) > maxQueryValues {
					return InvalidArgument("query parameter %q repeats more than %d times", f.name, maxQueryValues)
				}
				sl := reflect.MakeSlice(f.typ, len(vals), len(vals))
				for i, s := range vals {
					if err := setScalar(sl.Index(i), s); err != nil {
						return InvalidArgument("invalid value for query parameter %q: %v", f.name, err)
					}
				}
				fv.Set(sl)
				continue
			}
			if err := setScalar(fv, vals[0]); err != nil {
				return InvalidArgument("invalid value for query parameter %q: %v", f.name, err)
			}
		}
	}
	if !m.hasBody || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	body := http.MaxBytesReader(w, r.Body, opts.maxBody)
	defer func() { _ = body.Close() }()
	// Peek to treat an empty body as "no body" whatever the Content-Length says.
	br := &peekReader{r: body}
	if empty, err := br.empty(); err != nil {
		return bodyError(err)
	} else if empty {
		return nil
	}
	if ct := r.Header.Get("Content-Type"); !isJSONContentType(ct) {
		return &Error{Status: http.StatusUnsupportedMediaType, Code: apiclient.CodeInvalidArgument, Detail: "Invalid content type: the request body must be application/json"}
	}
	target := dst
	if m.bodyIndex != nil {
		target = dst.FieldByIndex(m.bodyIndex)
	}
	dec := json.NewDecoder(br)
	if !opts.allowUnknown {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(target.Addr().Interface()); err != nil {
		return bodyError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return bodyError(err)
		}
		return InvalidArgument("invalid request body: unexpected data after the JSON value")
	}
	return nil
}

func parseQuery(r *http.Request) (map[string][]string, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, InvalidArgument("invalid query string: %v", err)
	}
	return q, nil
}

func bodyError(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return &Error{Status: http.StatusRequestEntityTooLarge, Code: apiclient.CodeResourceExhausted, Detail: "request body too large", Cause: err}
	}
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		return InvalidArgument("invalid request body: malformed JSON at offset %d", syn.Offset)
	case errors.As(err, &typ):
		field := typ.Field
		if field == "" {
			field = "(root)"
		}
		return InvalidArgument("invalid request body: field %s must be %s", field, typ.Type)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return InvalidArgument("invalid request body: truncated JSON")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return InvalidArgument("invalid request body: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return InvalidArgument("invalid request body: %v", err).WithCause(err)
}

func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/json" || (strings.HasPrefix(mt, "application/") && strings.HasSuffix(mt, "+json"))
}

// peekReader reports whether a body is empty without losing the first byte.
type peekReader struct {
	r      io.Reader
	first  []byte
	peeked bool
}

func (p *peekReader) empty() (bool, error) {
	p.peeked = true
	buf := make([]byte, 1)
	for {
		n, err := p.r.Read(buf)
		if n > 0 {
			p.first = buf[:n]
			return false, nil
		}
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func (p *peekReader) Read(b []byte) (int, error) {
	if len(p.first) > 0 {
		n := copy(b, p.first)
		p.first = p.first[n:]
		return n, nil
	}
	return p.r.Read(b)
}
