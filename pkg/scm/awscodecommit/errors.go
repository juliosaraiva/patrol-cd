package awscodecommit

import (
	"context"
	"errors"
	"net/http"
	"strings"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// notFoundCodes are AWS error codes meaning that the addressed object does not exist.
var notFoundCodes = []string{
	"RepositoryDoesNotExistException", "BranchDoesNotExistException", "CommitDoesNotExistException",
	"FolderDoesNotExistException", "FileDoesNotExistException", "ReferenceDoesNotExistException",
	"ResourceNotFoundException",
}

var throttleCodes = []string{
	"ThrottlingException", "Throttling", "TooManyRequestsException", "RequestLimitExceeded",
	"ThrottledException", "RequestThrottledException", "SlowDown",
}

var unauthorizedCodes = []string{
	"UnrecognizedClientException", "InvalidSignatureException", "SignatureDoesNotMatch",
	"ExpiredTokenException", "ExpiredToken", "InvalidClientTokenId", "MissingAuthenticationToken",
	"IncompleteSignature",
}

var forbiddenCodes = []string{
	"AccessDeniedException", "AccessDenied", "UnauthorizedOperation", "EncryptionKeyAccessDeniedException",
	"EncryptionKeyDisabledException", "EncryptionKeyNotFoundException", "EncryptionKeyUnavailableException",
}

// mapError normalizes an AWS SDK error into *scm.Error.
func mapError(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var se *scm.Error
	if errors.As(err, &se) {
		c := *se
		if c.Provider == "" {
			c.Provider = Type
		}
		if c.Op == "" || c.Op == "Paginate" {
			c.Op = op
		}
		return &c
	}
	e := &scm.Error{Provider: Type, Op: op, Err: err}
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		e.StatusCode = re.HTTPStatusCode()
	}
	var api smithy.APIError
	switch {
	case ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		return e // keep the context error recognizable; no kind
	case errors.Is(err, scm.ErrResponseTooLarge):
		e.Kind = scm.KindInvalid
	case errors.As(err, &api):
		e.Kind = kindForCode(api.ErrorCode(), e.StatusCode)
	case strings.Contains(err.Error(), "failed to retrieve credentials") || strings.Contains(err.Error(), "failed to refresh cached credentials"):
		e.Kind = scm.KindUnauthorized
	default:
		e.Kind = scm.KindUnavailable // network failures, 5xx without an error code
	}
	if e.Kind == scm.KindRateLimited {
		e.RetryAfter = throttleRetryAfter
	}
	return e
}

func kindForCode(code string, status int) scm.ErrorKind {
	switch {
	case containsCode(notFoundCodes, code):
		return scm.KindNotFound
	case containsCode(throttleCodes, code), status == http.StatusTooManyRequests:
		return scm.KindRateLimited
	case containsCode(unauthorizedCodes, code):
		return scm.KindUnauthorized
	case containsCode(forbiddenCodes, code):
		return scm.KindForbidden
	case strings.HasPrefix(code, "Invalid") || strings.HasSuffix(code, "RequiredException") ||
		code == "ValidationException" || strings.HasSuffix(code, "ExceededException"):
		return scm.KindInvalid
	case status >= http.StatusInternalServerError || code == "ServiceUnavailable" || code == "InternalFailure":
		return scm.KindUnavailable
	}
	return scm.KindUnavailable
}

func containsCode(codes []string, code string) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}
