package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// Connection state messages (upstream server/repository).
const (
	// MessageConfigurationError is the connection message for configuration problems; the
	// details are in the Ready condition and the logs.
	MessageConfigurationError = "Configuration error - please check the server logs"
	// MessageConnectionFailedPrefix prefixes connection failures.
	MessageConnectionFailedPrefix = "Unable to connect to repository: "
)

// ErrNoRepoServer is returned by Validate when the Resolver has no repo-server client.
var ErrNoRepoServer = errors.New("no repo-server client configured")

// ErrStaleConnectionState is returned by WriteConnectionState when the result no longer
// describes the stored object: its generation or UID differs from the validated object's,
// or the stored connection state is from a later check. Nothing is written; validate the
// current object again if needed.
var ErrStaleConnectionState = errors.New("stale connection state")

// maxConditionMessage is the metav1.Condition message limit.
const maxConditionMessage = 32768

// TestRepository runs the repo-server connectivity test (TestRepository) for resolved
// access material. A response with verifiedRepository=false is an error. Secret values
// echoed in the error are redacted.
func (r *Resolver) TestRepository(ctx context.Context, repo *apiclient.Repository) error {
	if r.repoServer == nil {
		return ErrNoRepoServer
	}
	if repo == nil {
		return configError(ErrInvalidConfiguration, "nil repository")
	}
	resp, err := r.repoServer.TestRepository(ctx, &apiclient.TestRepositoryRequest{Repo: *repo})
	if err == nil && (resp == nil || !resp.VerifiedRepository) {
		err = errors.New("repository could not be verified")
	}
	return redact(err, repositorySecrets(repo))
}

// repositorySecrets lists the secret values of resolved access material.
func repositorySecrets(r *apiclient.Repository) []string {
	return []string{r.Password, r.BearerToken, r.SSHPrivateKey, r.TLSClientCertKey}
}

// Validate resolves the access of a stored Repository (with template inheritance) and tests
// it on the repo-server. It returns the connection state to record (upstream message
// formats: "Unable to connect to repository: <error>", or MessageConfigurationError) and the
// underlying error, nil on success. The state's AttemptedAt is the resolver clock.
func (r *Resolver) Validate(ctx context.Context, repo *v1alpha1.Repository) (v1alpha1.ConnectionState, error) {
	return r.validate(ctx, repo, nil, true)
}

// ValidateWithSecretData validates unsaved input (upstream ValidateAccess /
// ValidateWriteAccess): data replaces the Secret named by spec.credentialsRef. Credential
// templates are applied to read repositories without credentials, not to write ones (as
// upstream ValidateWriteAccess).
func (r *Resolver) ValidateWithSecretData(ctx context.Context, repo *v1alpha1.Repository, data map[string][]byte) (v1alpha1.ConnectionState, error) {
	if data == nil {
		data = map[string][]byte{}
	}
	templates := repo == nil || purposeKey(repo.Spec.Purpose) == string(v1alpha1.RepositoryPurposeRead)
	return r.validate(ctx, repo, data, templates)
}

func (r *Resolver) validate(ctx context.Context, repo *v1alpha1.Repository, inline map[string][]byte, templates bool) (v1alpha1.ConnectionState, error) {
	now := metav1.NewTime(r.now())
	state := v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusFailed, AttemptedAt: &now}
	fail := func(err error) (v1alpha1.ConnectionState, error) {
		if IsConfigurationError(err) {
			state.Message = MessageConfigurationError
		} else {
			state.Message = truncateCondition(MessageConnectionFailedPrefix + err.Error())
		}
		r.metrics.validation(string(v1alpha1.ConnectionStatusFailed))
		name := ""
		if repo != nil {
			name = repo.Name
		}
		r.logger.Info("repository connection check failed", "repository", name, "reason", ConditionReason(err), "error", err)
		return state, err
	}
	if r.repoServer == nil {
		return fail(ErrNoRepoServer)
	}
	var typeHint v1alpha1.RepositoryType
	if repo != nil {
		typeHint = repo.Spec.Type
	}
	res, err := r.resolveObject(ctx, repo, inline, templates, typeHint)
	if err != nil {
		return fail(err)
	}
	if err := r.TestRepository(ctx, res.Repository); err != nil {
		return fail(err)
	}
	state.Status = v1alpha1.ConnectionStatusSuccessful
	r.metrics.validation(string(v1alpha1.ConnectionStatusSuccessful))
	return state, nil
}

// StatusWriter persists Repository.status.connectionState and the Ready and
// ConnectionFailed conditions through the status subresource, re-reading the object and
// retrying on conflicts and transient API errors.
type StatusWriter struct {
	client  dynamic.Interface
	backoff wait.Backoff
	now     func() time.Time
}

// DefaultStatusBackoff retries five times starting at 100ms, doubling, with jitter.
var DefaultStatusBackoff = wait.Backoff{Steps: 5, Duration: 100 * time.Millisecond, Factor: 2, Jitter: 0.1, Cap: 5 * time.Second}

// StatusWriterOption configures a StatusWriter.
type StatusWriterOption func(*StatusWriter)

// WithStatusBackoff replaces DefaultStatusBackoff.
func WithStatusBackoff(b wait.Backoff) StatusWriterOption {
	return func(w *StatusWriter) { w.backoff = b }
}

// WithStatusClock replaces time.Now for condition transition times.
func WithStatusClock(now func() time.Time) StatusWriterOption {
	return func(w *StatusWriter) { w.now = now }
}

// NewStatusWriter returns a writer using dyn.
func NewStatusWriter(dyn dynamic.Interface, opts ...StatusWriterOption) *StatusWriter {
	w := &StatusWriter{client: dyn, backoff: DefaultStatusBackoff, now: time.Now}
	for _, o := range opts {
		o(w)
	}
	return w
}

func retriable(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err)
}

// WriteConnectionState records the result of validating repo (state and cause as returned
// by Validate for that object) on the stored Repository, with cause deciding the condition
// reasons and messages. The write is skipped with ErrStaleConnectionState when the stored
// object is not the one that was validated (different generation or UID: its spec changed
// or it was recreated while the check ran) or already records a later check
// (connectionState.attemptedAt, compared at its stored one-second precision), so a slow
// check never overwrites a newer result nor reports a spec it did not test. It returns the
// stored object; a missing object returns the API NotFound error.
func (w *StatusWriter) WriteConnectionState(ctx context.Context, repo *v1alpha1.Repository, state v1alpha1.ConnectionState, cause error) (*v1alpha1.Repository, error) {
	if repo == nil || repo.Name == "" {
		return nil, errors.New("write connection state: repository name is required")
	}
	namespace, name := repo.Namespace, repo.Name
	res := w.client.Resource(RepositoryGVR).Namespace(namespace)
	var (
		out   *v1alpha1.Repository
		stale string
	)
	err := retry.OnError(w.backoff, retriable, func() error {
		stale = ""
		u, err := res.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		current := &v1alpha1.Repository{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, current); err != nil {
			return fmt.Errorf("decode repository: %w", err)
		}
		out = current
		if stale = staleness(repo, current, state); stale != "" {
			return nil
		}
		before := current.Status.DeepCopy()
		applyConnectionStatus(&current.Status, repo.Generation, state, cause, w.now())
		if equality.Semantic.DeepEqual(before, &current.Status) {
			return nil
		}
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(current)
		if err != nil {
			return fmt.Errorf("encode repository: %w", err)
		}
		nu := &unstructured.Unstructured{Object: obj}
		nu.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Repository"))
		stored, err := res.UpdateStatus(ctx, nu, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		sr := &v1alpha1.Repository{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, sr); err != nil {
			return fmt.Errorf("decode repository: %w", err)
		}
		out = sr
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("update status of repository %s/%s: %w", namespace, name, err)
	}
	if stale != "" {
		return out, fmt.Errorf("update status of repository %s/%s: %s: %w", namespace, name, stale, ErrStaleConnectionState)
	}
	return out, nil
}

// staleness explains why state (a check of validated) must not be recorded on current, or
// returns "".
func staleness(validated, current *v1alpha1.Repository, state v1alpha1.ConnectionState) string {
	switch {
	case validated.UID != "" && current.UID != validated.UID:
		return "the repository was recreated after it was validated"
	case current.Generation != validated.Generation:
		return fmt.Sprintf("generation %d was validated but the repository is at generation %d", validated.Generation, current.Generation)
	}
	prev := current.Status.ConnectionState
	if prev != nil && prev.AttemptedAt != nil && state.AttemptedAt != nil &&
		prev.AttemptedAt.After(state.AttemptedAt.Truncate(time.Second)) {
		return "a later connection check is already recorded"
	}
	return ""
}

func truncateCondition(msg string) string {
	if len(msg) <= maxConditionMessage {
		return msg
	}
	return strings.ToValidUTF8(msg[:maxConditionMessage], "")
}

// applyConnectionStatus sets connectionState, observedGeneration and the Ready and
// ConnectionFailed conditions.
func applyConnectionStatus(st *v1alpha1.RepositoryStatus, generation int64, state v1alpha1.ConnectionState, cause error, now time.Time) {
	cs := state
	st.ConnectionState = &cs
	st.ObservedGeneration = generation
	ready := metav1.Condition{Type: v1alpha1.ConditionReady, ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now)}
	failed := metav1.Condition{Type: v1alpha1.ConditionConnectionFailed, ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now)}
	msg := state.Message
	if cause != nil {
		msg = cause.Error()
	}
	msg = truncateCondition(msg)
	switch state.Status {
	case v1alpha1.ConnectionStatusSuccessful:
		ready.Status, ready.Reason = metav1.ConditionTrue, v1alpha1.ReasonConnectionSuccessful
		failed.Status, failed.Reason = metav1.ConditionFalse, v1alpha1.ReasonConnectionSuccessful
	case v1alpha1.ConnectionStatusFailed:
		reason := ConditionReason(cause)
		if cause == nil {
			reason = v1alpha1.ReasonConnectionFailed
		}
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, reason, msg
		failed.Status, failed.Reason, failed.Message = metav1.ConditionTrue, reason, msg
	default:
		ready.Status, ready.Reason = metav1.ConditionUnknown, v1alpha1.ReasonCheckPending
		failed.Status, failed.Reason = metav1.ConditionUnknown, v1alpha1.ReasonCheckPending
	}
	meta.SetStatusCondition(&st.Conditions, ready)
	meta.SetStatusCondition(&st.Conditions, failed)
}
