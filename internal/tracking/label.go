package tracking

import "errors"

// LabelMaxLength is the maximum length of a Kubernetes label value.
const LabelMaxLength = 63

// ErrLabelTruncation is returned by [TruncateLabel] when no alphanumeric character is left
// to end the truncated value with. The message is the upstream one.
var ErrLabelTruncation = errors.New("unable to truncate label to not end with a special character")

// TruncateLabel shortens val to fit a label value. Values of at most [LabelMaxLength] bytes
// are returned unchanged (not validated). Longer values are cut to LabelMaxLength bytes and
// then trailing characters other than [a-zA-Z0-9] are removed; if none remains,
// [ErrLabelTruncation] is returned.
func TruncateLabel(val string) (string, error) {
	if len(val) <= LabelMaxLength {
		return val, nil
	}
	val = val[:LabelMaxLength]
	for !endsAlphanumeric(val) {
		if len(val) <= 1 {
			return "", ErrLabelTruncation
		}
		val = val[:len(val)-1]
	}
	return val, nil
}

func endsAlphanumeric(s string) bool {
	if s == "" {
		return false
	}
	c := s[len(s)-1]
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}
