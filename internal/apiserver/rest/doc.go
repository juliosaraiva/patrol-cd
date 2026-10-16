// Package rest is the HTTP framework of patrol-server's public API (ADR-0005,
// docs/architecture/api-contract.md).
//
// A Table holds Routes. Each route declares its method and Go 1.22 ServeMux path
// pattern, operation ID, documentation, access rule (public, authenticated, or an RBAC
// check), request and response Go types, and whether it streams Server-Sent Events. The
// same table drives request routing (Table.Mount) and the generated OpenAPI 3.1 document
// (internal/apiserver/openapi), so the two cannot disagree.
//
// Typed JSON handlers are registered with Register:
//
//	rest.Register(t, rest.Route{
//		Method: http.MethodGet, Path: "/api/v1/account/{name}", OperationID: "getAccount",
//		Access: rest.RBACInHandler(rbac.ResourceAccounts, rbac.ActionGet, "{name}"),
//	}, h.getAccount) // func(ctx context.Context, req *apiclient.GetAccountRequest) (*apiclient.Account, error)
//
// Request decoding binds `path:"…"` and `query:"…"` tagged fields, then the JSON body
// (size-capped; unknown fields rejected except for Kubernetes object bodies). Handler
// errors become RFC 9457 problem documents (see Error and ProblemFor); Kubernetes API
// errors, RBAC denials and token errors are mapped automatically.
//
// Every mounted route runs this chain (outermost first): authentication (bearer or
// patrol.token cookie, token renewal, anonymous access), CSRF protection for
// cookie-authenticated mutations, login rate limiting, RBAC enforcement, then audit
// logging around the handler. Process-wide middleware (panic recovery, request IDs,
// access log, security headers, body size cap) is provided by Chain for the server to
// wrap its whole handler.
package rest
