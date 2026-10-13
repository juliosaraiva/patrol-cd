package notifications

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// MaxDeliveryStateEntries bounds the delivery-state annotation; the oldest entries are
// pruned first (baseline: 100).
const MaxDeliveryStateEntries = 100

// maxStateAnnotationBytes guards against parsing a hostile, oversized annotation value.
const maxStateAnnotationBytes = 256 << 10

// DeliveryState records which (trigger, condition, destination[, oncePer value]) tuples
// were notified, mapping
//
//	[<namespace>:]<trigger>:<conditionKey>:<service>:<recipient>
//
// (prefixed with "<oncePer>:" when the condition has oncePer) to the Unix time of the
// delivery. It is persisted as JSON in the delivery-state annotation.
//
// Semantics (baseline compatible):
//   - a triggered condition is delivered once per key;
//   - when the condition stops being true its key is removed, so the next time it becomes
//     true it is delivered again;
//   - keys of conditions with oncePer are never removed when the condition becomes false,
//     so a new delivery happens only for a new oncePer value;
//   - failed deliveries are not recorded (they are retried on the next evaluation; see
//     component docs for the difference with the baseline for oncePer conditions);
//   - the state is truncated to MaxDeliveryStateEntries, dropping the oldest entries.
type DeliveryState map[string]int64

// ParseDeliveryState decodes an annotation value. Invalid or oversized values yield an
// empty state (baseline behavior: start over rather than fail).
func ParseDeliveryState(v string) DeliveryState {
	if v == "" || len(v) > maxStateAnnotationBytes {
		return DeliveryState{}
	}
	var s DeliveryState
	if err := json.Unmarshal([]byte(v), &s); err != nil || s == nil {
		return DeliveryState{}
	}
	return s
}

// DeliveryStateFromAnnotations reads the delivery state for an annotation prefix.
func DeliveryStateFromAnnotations(annotations map[string]string, prefix string) DeliveryState {
	if annotations == nil {
		return DeliveryState{}
	}
	return ParseDeliveryState(annotations[DeliveryStateAnnotation(prefix)])
}

// Clone returns a copy of the state (never nil).
func (s DeliveryState) Clone() DeliveryState {
	out := make(DeliveryState, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// Truncate removes the oldest entries beyond max (ties broken by key for determinism).
func (s DeliveryState) Truncate(maxEntries int) {
	if len(s) <= maxEntries {
		return
	}
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if s[keys[i]] != s[keys[j]] {
			return s[keys[i]] < s[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys[:len(keys)-maxEntries] {
		delete(s, k)
	}
}

// Encode returns the annotation value ("" for an empty state). The receiver is not
// modified; the encoded copy is truncated to MaxDeliveryStateEntries.
func (s DeliveryState) Encode() (string, error) {
	if len(s) == 0 {
		return "", nil
	}
	cp := s.Clone()
	cp.Truncate(MaxDeliveryStateEntries)
	b, err := json.Marshal(cp)
	if err != nil {
		return "", fmt.Errorf("encoding delivery state: %w", err)
	}
	return string(b), nil
}

// ApplyDeliveryState returns a copy of annotations with the delivery-state annotation set
// to s (or removed when s is empty).
func ApplyDeliveryState(annotations map[string]string, prefix string, s DeliveryState) (map[string]string, error) {
	out := make(map[string]string, len(annotations)+1)
	for k, v := range annotations {
		out[k] = v
	}
	key := DeliveryStateAnnotation(prefix)
	enc, err := s.Encode()
	if err != nil {
		return nil, err
	}
	if enc == "" {
		delete(out, key)
	} else {
		out[key] = enc
	}
	return out, nil
}

// StateKey builds the delivery-state key for a condition result and destination.
func StateKey(namespace, trigger string, cr ConditionResult, dest services.Destination) string {
	key := fmt.Sprintf("%s:%s:%s:%s", trigger, cr.Key, dest.Service, dest.Recipient)
	if namespace != "" {
		key = namespace + ":" + key
	}
	if cr.OncePer != "" {
		key = cr.OncePer + ":" + key
	}
	return key
}

// setNotified records (notified=true) or clears (notified=false) a key and reports whether
// the state changed. Clearing is a no-op for oncePer conditions.
func (s DeliveryState) setNotified(key string, oncePer bool, notified bool, now int64) bool {
	_, already := s[key]
	if already == notified {
		return false
	}
	if notified {
		s[key] = now
		return true
	}
	if oncePer {
		return false
	}
	delete(s, key)
	return true
}
