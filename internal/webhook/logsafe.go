package webhook

import (
	"strconv"
	"unicode/utf8"
)

// Bounds of untrusted text in log records. Payload fields (ref, action, repository URL,
// package tag) and error messages that may quote headers or payload values are clipped,
// so that an unauthenticated sender cannot make one delivery write megabytes of logs.
const (
	maxLogValueBytes = 256
	maxLogErrorBytes = 1024
)

// clip returns s when it fits in max bytes, otherwise its longest prefix of at most max
// bytes that ends on a UTF-8 boundary followed by a marker with the number of dropped
// bytes.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "...[truncated " + strconv.Itoa(len(s)-cut) + " bytes]"
}

// logValue bounds an untrusted string for logging.
func logValue(s string) string { return clip(s, maxLogValueBytes) }

// logErr bounds an error message for logging ("" for nil).
func logErr(err error) string {
	if err == nil {
		return ""
	}
	return clip(err.Error(), maxLogErrorBytes)
}
