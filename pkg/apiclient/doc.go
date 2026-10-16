// Package apiclient is the Go client of the Patrol CD public HTTP API (`/api/v1`, see
// docs/architecture/api-contract.md and api/openapi/openapi.json).
//
// The request and response types in this package are shared with the server
// (internal/apiserver), so client and server cannot drift. Request types use struct tags
// to place fields:
//
//   - `path:"name"` binds a path parameter of the route pattern ("{name}", or the
//     trailing "{name...}" wildcard);
//   - `query:"name"` binds a query parameter (scalars, pointers to scalars for presence,
//     and slices, which repeat the key);
//   - `body:"true"` marks the one field that holds the request body (used when the body
//     is a whole object such as an Application);
//   - every other JSON-visible field belongs to the JSON body. Path and query fields
//     must be tagged `json:"-"`.
//
// Client.Do builds a request from such a type, so a resource client is a one-liner:
//
//	func (c *Client) GetAccount(ctx context.Context, name string) (*Account, error) {
//		var out Account
//		err := c.Do(ctx, http.MethodGet, "/api/v1/account/{name}", &GetAccountRequest{Name: name}, &out)
//		return &out, err
//	}
//
// Errors returned by the server are *APIError values carrying the RFC 9457 problem
// details; use IsNotFound, IsPermissionDenied, IsUnauthenticated and friends, or
// errors.As. Server-Sent Event streams are read with Client.Stream.
//
// Flags that only make sense for Argo CD's gRPC transport (gRPC-Web, port forwarding,
// core mode) are accepted by ClientOptions and ignored, except GRPCWebRootPath, which
// is honored as the root path.
package apiclient
