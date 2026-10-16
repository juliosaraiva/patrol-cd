package scm

import (
	"container/list"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MaxWebhookPayloadBytes is the default cap for webhook bodies (GitHub caps payloads at
// 25 MiB). Callers should apply their configured limit with ReadWebhookBody.
const MaxWebhookPayloadBytes = 25 << 20

// Webhook verification errors. Providers return them wrapped in *Error (Kind
// Unauthorized for signature problems, Invalid otherwise).
var (
	ErrSignatureMissing  = errors.New("scm: webhook signature missing")
	ErrSignatureMismatch = errors.New("scm: webhook signature mismatch")
	ErrPayloadTooLarge   = errors.New("scm: webhook payload too large")
	ErrMalformedPayload  = errors.New("scm: malformed webhook payload")
	ErrUnsupportedEvent  = errors.New("scm: unsupported webhook event")
	// ErrInvalidDeliveryID means a delivery ID header is longer than
	// MaxDeliveryIDBytes or contains bytes other than printable, non-space ASCII.
	ErrInvalidDeliveryID = errors.New("scm: invalid webhook delivery ID")
)

// MaxDeliveryIDBytes caps webhook delivery IDs. Providers send short identifiers
// (GitHub, Gitea and Gogs: 36-character GUIDs; GitLab: UUIDs), so anything longer is
// not a genuine delivery.
const MaxDeliveryIDBytes = 128

// ValidateDeliveryID checks an untrusted delivery ID header value: at most
// MaxDeliveryIDBytes of printable ASCII without spaces. The empty ID (header absent) is
// valid and means "no de-duplication". Providers call it from VerifyAndParse and fail
// with KindInvalid wrapping the returned error.
func ValidateDeliveryID(id string) error {
	if len(id) > MaxDeliveryIDBytes {
		return fmt.Errorf("%w: %d bytes exceeds the %d-byte limit", ErrInvalidDeliveryID, len(id), MaxDeliveryIDBytes)
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c <= ' ' || c > '~' {
			return fmt.Errorf("%w: byte 0x%02x at offset %d is not printable ASCII", ErrInvalidDeliveryID, c, i)
		}
	}
	return nil
}

// EventType is a normalized webhook event type.
type EventType string

// Normalized event types.
const (
	EventPush         EventType = "push"
	EventPullRequest  EventType = "pull_request"
	EventInstallation EventType = "installation"
	EventRepository   EventType = "repository"
	EventPing         EventType = "ping"
)

// Normalized pull request actions (GitHub vocabulary; other providers map onto it, and
// unmapped actions pass through verbatim).
const (
	PRActionOpened      = "opened"
	PRActionSynchronize = "synchronize"
	PRActionReopened    = "reopened"
	PRActionClosed      = "closed"
	PRActionLabeled     = "labeled"
	PRActionUnlabeled   = "unlabeled"
	PRActionEdited      = "edited"
)

// EventRepo is the repository a webhook event refers to.
type EventRepo struct {
	RepoRef
	ID            string
	HTMLURL       string
	CloneURL      string
	SSHURL        string
	APIURL        string
	DefaultBranch string
	Private       bool
}

// Event is a normalized, verified (or explicitly unverified) webhook delivery.
type Event struct {
	// Provider is the provider type that parsed the event.
	Provider string
	Type     EventType
	// DeliveryID is the provider's delivery ID header, used to de-duplicate
	// redeliveries (see DeliveryCache). It is NOT covered by the webhook signature:
	// anyone holding one signed body can resend it under any ID. Providers reject IDs
	// failing ValidateDeliveryID; empty means the header was absent.
	DeliveryID string
	// Verified is true only when a secret was configured and the signature matched.
	Verified bool
	// Action is the event action (pull_request, installation, repository events).
	Action     string
	Repository *EventRepo
	// Ref is the full ref of a push ("refs/heads/main"); Revision is its short form.
	Ref      string
	Revision string
	Before   string
	After    string
	Created  bool
	Deleted  bool
	Forced   bool
	// TouchedDefaultBranch reports a push to the repository's default branch.
	TouchedDefaultBranch bool
	// ChangedFiles lists added, modified and removed paths of a push, de-duplicated in
	// first-seen order. ChangedFilesTruncated is set when the list was capped; callers
	// must then treat every path as changed.
	ChangedFiles          []string
	ChangedFilesTruncated bool
	PullRequest           *PullRequest
	Installation          *InstallationRef
	RepositoriesAdded     []RepoRef
	RepositoriesRemoved   []RepoRef
	Sender                string
}

// MaxChangedFiles caps Event.ChangedFiles.
const MaxChangedFiles = 10000

// ReadWebhookBody reads at most maxBytes from r (MaxWebhookPayloadBytes when <= 0) and
// fails with ErrPayloadTooLarge (KindInvalid) when the body is larger.
func ReadWebhookBody(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = MaxWebhookPayloadBytes
	}
	b, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, &Error{Kind: KindInvalid, Op: "ReadWebhookBody", Err: err}
	}
	if int64(len(b)) > maxBytes {
		return nil, &Error{Kind: KindInvalid, Op: "ReadWebhookBody", Err: fmt.Errorf("%w: limit %d bytes", ErrPayloadTooLarge, maxBytes)}
	}
	return b, nil
}

// SignHMACSHA256 returns the lowercase hex HMAC-SHA256 of body.
func SignHMACSHA256(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyHMACSHA256 verifies a hex-encoded HMAC-SHA256 signature header value (optionally
// carrying prefix, e.g. "sha256=") over the raw body in constant time.
func VerifyHMACSHA256(header, prefix string, body, secret []byte) error {
	header = strings.TrimSpace(header)
	if header == "" {
		return ErrSignatureMissing
	}
	if prefix != "" {
		if !strings.HasPrefix(header, prefix) {
			return ErrSignatureMismatch
		}
		header = header[len(prefix):]
	}
	got, err := hex.DecodeString(header)
	if err != nil {
		return ErrSignatureMismatch
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	if !hmac.Equal(got, m.Sum(nil)) {
		return ErrSignatureMismatch
	}
	return nil
}

// ParseRevision returns the short name of a ref: "refs/heads/feature/x" → "feature/x",
// "refs/tags/v1" → "v1"; other values are returned unchanged (mirrors upstream).
func ParseRevision(ref string) string {
	parts := strings.SplitN(ref, "/", 3)
	return parts[len(parts)-1]
}

// NormalizeChangedFiles de-duplicates paths preserving first-seen order, dropping empty
// entries, and caps the result at MaxChangedFiles (truncated=true when capped).
func NormalizeChangedFiles(groups ...[]string) (files []string, truncated bool) {
	seen := make(map[string]struct{})
	for _, g := range groups {
		for _, f := range g {
			if f == "" {
				continue
			}
			if _, ok := seen[f]; ok {
				continue
			}
			if len(files) >= MaxChangedFiles {
				return files, true
			}
			seen[f] = struct{}{}
			files = append(files, f)
		}
	}
	return files, false
}

// DeliveryKey builds the de-duplication key for a delivery: provider-scoped, or empty
// (never recorded) when deliveryID is empty or fails ValidateDeliveryID.
func DeliveryKey(provider, deliveryID string) string {
	if deliveryID == "" || ValidateDeliveryID(deliveryID) != nil {
		return ""
	}
	return provider + "\x00" + deliveryID
}

// DeliveryCache is a bounded LRU of recently seen webhook delivery keys with a TTL. It is
// safe for concurrent use, and its memory is bounded by its capacity alone: keys are
// stored as fixed-size SHA-256 digests, whatever their length.
//
// It de-duplicates benign redeliveries (a provider retrying, an operator pressing
// "redeliver"); it is not replay protection. Delivery IDs are unsigned headers, so a
// party holding one signed body can resend it under fresh IDs, and webhook handling must
// stay idempotent. Record only events that passed signature verification (SeenEvent
// does this): unverified events can be sent by anyone, and recording them would only let
// unauthenticated senders evict legitimate entries. Because SCM redeliveries are rare and
// effects are idempotent, the cache is per replica and in memory.
type DeliveryCache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	now      func() time.Time
	ll       *list.List
	items    map[deliveryDigest]*list.Element
}

type deliveryDigest [sha256.Size]byte

type deliveryEntry struct {
	key  deliveryDigest
	seen time.Time
}

// DeliveryCacheOption configures a DeliveryCache.
type DeliveryCacheOption func(*DeliveryCache)

// WithDeliveryClock overrides time.Now.
func WithDeliveryClock(now func() time.Time) DeliveryCacheOption {
	return func(c *DeliveryCache) { c.now = now }
}

// NewDeliveryCache returns a cache holding at most capacity keys (default 10000) for ttl
// (default 1h).
func NewDeliveryCache(capacity int, ttl time.Duration, opts ...DeliveryCacheOption) *DeliveryCache {
	if capacity <= 0 {
		capacity = 10000
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	c := &DeliveryCache{capacity: capacity, ttl: ttl, now: time.Now, ll: list.New(), items: map[deliveryDigest]*list.Element{}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// SeenEvent records a verified event's delivery and reports whether it was already
// recorded within the TTL. Unverified events (no secret configured) and events without
// a valid delivery ID are never recorded and report false.
func (c *DeliveryCache) SeenEvent(ev *Event) bool {
	if ev == nil || !ev.Verified {
		return false
	}
	return c.Seen(DeliveryKey(ev.Provider, ev.DeliveryID))
}

// Seen records key and reports whether it was already recorded within the TTL. Empty
// keys are never recorded and always report false.
func (c *DeliveryCache) Seen(key string) bool {
	if key == "" {
		return false
	}
	d := deliveryDigest(sha256.Sum256([]byte(key)))
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if el, ok := c.items[d]; ok {
		e := el.Value.(*deliveryEntry)
		if now.Sub(e.seen) < c.ttl {
			c.ll.MoveToFront(el)
			return true
		}
		e.seen = now
		c.ll.MoveToFront(el)
		return false
	}
	c.items[d] = c.ll.PushFront(&deliveryEntry{key: d, seen: now})
	for c.ll.Len() > c.capacity {
		c.removeElement(c.ll.Back())
	}
	return false
}

// Forget removes key so a redelivery is processed again (call it when processing of a
// recorded delivery failed).
func (c *DeliveryCache) Forget(key string) {
	if key == "" {
		return
	}
	d := deliveryDigest(sha256.Sum256([]byte(key)))
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[d]; ok {
		c.removeElement(el)
	}
}

// Len returns the number of recorded keys (including expired ones not yet evicted).
func (c *DeliveryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *DeliveryCache) removeElement(el *list.Element) {
	if el == nil {
		return
	}
	c.ll.Remove(el)
	delete(c.items, el.Value.(*deliveryEntry).key)
}

// WebhookSignatureError wraps a signature error as an Unauthorized *Error.
func WebhookSignatureError(provider string, err error) error {
	return &Error{Kind: KindUnauthorized, Provider: provider, Op: "VerifyAndParse", StatusCode: http.StatusUnauthorized, Err: err}
}
