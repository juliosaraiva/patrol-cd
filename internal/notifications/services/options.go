package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// flexString accepts a JSON string, number or boolean and stores its textual form. Service
// options frequently mix literal numbers with $secret references that render as strings.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*f = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*f = flexString(n.String())
		return nil
	}
	var v bool
	if err := json.Unmarshal(b, &v); err == nil {
		*f = flexString(strconv.FormatBool(v))
		return nil
	}
	return fmt.Errorf("expected a string or number, got %s", string(b))
}

func (f flexString) int64() (int64, error) {
	s := strings.TrimSpace(string(f))
	if s == "" {
		return 0, nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, nil
	}
	fl, err := strconv.ParseFloat(s, 64)
	if err != nil || fl != float64(int64(fl)) {
		return 0, fmt.Errorf("%q is not an integer", s)
	}
	return int64(fl), nil
}

// flexBool accepts a JSON bool or a string such as "true".
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*f = false
		return nil
	}
	var v bool
	if err := json.Unmarshal(b, &v); err == nil {
		*f = flexBool(v)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("expected a boolean, got %s", string(b))
	}
	if strings.TrimSpace(s) == "" {
		*f = false
		return nil
	}
	v, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("expected a boolean, got %q", s)
	}
	*f = flexBool(v)
	return nil
}

// parseFlexDuration parses Go duration strings; bare integers are seconds.
func parseFlexDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("negative duration %q", s)
	}
	return d, nil
}

// TransportOptions are the per-service HTTP connection settings of the baseline.
type TransportOptions struct {
	InsecureSkipVerify  flexBool   `json:"insecureSkipVerify"`
	MaxIdleConns        flexString `json:"maxIdleConns"`
	MaxIdleConnsPerHost flexString `json:"maxIdleConnsPerHost"`
	MaxConnsPerHost     flexString `json:"maxConnsPerHost"`
	IdleConnTimeout     flexString `json:"idleConnTimeout"`
}

type transportSettings struct {
	insecureSkipVerify  bool
	maxIdleConns        int
	maxIdleConnsPerHost int
	maxConnsPerHost     int
	idleConnTimeout     time.Duration
}

func (t TransportOptions) settings() (transportSettings, error) {
	var s transportSettings
	s.insecureSkipVerify = bool(t.InsecureSkipVerify)
	for _, f := range []struct {
		name string
		in   flexString
		out  *int
	}{
		{"maxIdleConns", t.MaxIdleConns, &s.maxIdleConns},
		{"maxIdleConnsPerHost", t.MaxIdleConnsPerHost, &s.maxIdleConnsPerHost},
		{"maxConnsPerHost", t.MaxConnsPerHost, &s.maxConnsPerHost},
	} {
		v, err := f.in.int64()
		if err != nil || v < 0 || v > 1<<20 {
			return s, fmt.Errorf("%w: %s: invalid value", ErrInvalidOptions, f.name)
		}
		*f.out = int(v)
	}
	d, err := parseFlexDuration(string(t.IdleConnTimeout))
	if err != nil {
		return s, fmt.Errorf("%w: idleConnTimeout: %v", ErrInvalidOptions, err)
	}
	s.idleConnTimeout = d
	return s, nil
}

// decodeOptions decodes JSON service options into dst. Unknown fields are ignored, as in
// the baseline.
func decodeOptions(serviceType string, raw []byte, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalidOptions, serviceType, err)
	}
	return nil
}
