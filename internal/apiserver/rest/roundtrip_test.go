package rest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// roundTrip is bound by both the Go client (pkg/apiclient) and the server decoder.
type roundTrip struct {
	Name  string   `path:"name" json:"-"`
	Rest  string   `path:"rest" json:"-"`
	Q     string   `query:"q" json:"-"`
	N     *int64   `query:"n" json:"-"`
	Tags  []string `query:"tag" json:"-"`
	Flag  bool     `query:"flag" json:"-"`
	Value string   `json:"value"`
	Items []string `json:"items,omitempty"`
}

type roundTripEcho struct {
	Name  string   `json:"name"`
	Rest  string   `json:"rest"`
	Q     string   `json:"q"`
	N     *int64   `json:"n"`
	Tags  []string `json:"tags"`
	Flag  bool     `json:"flag"`
	Value string   `json:"value"`
	Items []string `json:"items"`
}

// segment generates a non-empty path segment that is not "." or "..", which ServeMux
// would clean away (Kubernetes names never take these forms).
func segment() *rapid.Generator[string] {
	return rapid.StringMatching(`[^\x00]{1,12}`).Filter(func(s string) bool {
		return s != "." && s != ".." && !strings.Contains(s, "/")
	})
}

// TestClientServerRoundTripProperty checks that every request the client builds from a
// tagged struct decodes on the server into the same struct: path escaping (including
// slashes inside a single segment), multi-segment wildcards, repeated and optional query
// parameters, and JSON bodies.
func TestClientServerRoundTripProperty(t *testing.T) {
	t.Parallel()
	tbl := rest.NewTable()
	rest.Register(tbl, rest.Route{Method: http.MethodPost, Path: "/api/v1/rt/{name}/x/{rest...}", OperationID: "roundTrip", Access: rest.Public()},
		func(_ context.Context, r *roundTrip) (*roundTripEcho, error) {
			return &roundTripEcho{Name: r.Name, Rest: r.Rest, Q: r.Q, N: r.N, Tags: r.Tags, Flag: r.Flag, Value: r.Value, Items: r.Items}, nil
		})
	h, err := tbl.Handler(&rest.Env{DisableAuth: true})
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL})
	require.NoError(t, err)

	rapid.Check(t, func(rt *rapid.T) {
		in := roundTrip{
			Name:  rapid.StringMatching(`[^\x00]{1,24}`).Filter(func(s string) bool { return s != "." && s != ".." }).Draw(rt, "name"),
			Rest:  strings.Join(rapid.SliceOfN(segment(), 1, 4).Draw(rt, "rest"), "/"),
			Q:     rapid.String().Draw(rt, "q"),
			Tags:  rapid.SliceOfN(rapid.String().Filter(func(s string) bool { return s != "" }), 0, 3).Draw(rt, "tags"),
			Flag:  rapid.Bool().Draw(rt, "flag"),
			Value: rapid.String().Draw(rt, "value"),
			Items: rapid.SliceOfN(rapid.String(), 0, 3).Draw(rt, "items"),
		}
		if rapid.Bool().Draw(rt, "hasN") {
			n := rapid.Int64().Draw(rt, "n")
			in.N = &n
		}
		var out roundTripEcho
		require.NoError(rt, c.Do(context.Background(), http.MethodPost, "/api/v1/rt/{name}/x/{rest...}", &in, &out))
		if len(in.Tags) == 0 {
			in.Tags = nil
		}
		if len(in.Items) == 0 {
			in.Items = nil
		}
		want := roundTripEcho(in)
		require.Equal(rt, want, out)
	})
}
