package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Route patterns of the resource-level application API (api-contract.md, "Applications":
// resource tree, managed resources, server-side diff, events, live resources, resource
// actions, logs and the web terminal).
const (
	PathApplicationResourceTree       = "/api/v1/applications/{name}/resource-tree"
	PathStreamApplicationResourceTree = "/api/v1/stream/applications/{name}/resource-tree"
	PathApplicationManagedResources   = "/api/v1/applications/{name}/managed-resources"
	PathApplicationServerSideDiff     = "/api/v1/applications/{name}/server-side-diff"
	PathApplicationEvents             = "/api/v1/applications/{name}/events"
	PathApplicationResource           = "/api/v1/applications/{name}/resource"
	PathApplicationResourceActions    = "/api/v1/applications/{name}/resource/actions"
	PathApplicationLogs               = "/api/v1/applications/{name}/logs"
	PathApplicationPodLogs            = "/api/v1/applications/{name}/pods/{pod}/logs"
	// PathTerminal is the WebSocket endpoint of the web terminal (outside /api/v1, as
	// upstream; served under --rootpath).
	PathTerminal = "/terminal"
)

// PatchTypeStrategic selects a strategic merge patch (resource patches only).
const PatchTypeStrategic = "strategic"

// ApplicationTreeQuery addresses the resource tree of an Application.
type ApplicationTreeQuery struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
}

// ResourceRef identifies a resource in a resource tree.
type ResourceRef struct {
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	UID       string `json:"uid,omitempty"`
}

// InfoItem is a name/value pair shown for a resource.
type InfoItem struct {
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

// LoadBalancerIngress is one load balancer ingress point.
type LoadBalancerIngress struct {
	Hostname string `json:"hostname,omitempty"`
	IP       string `json:"ip,omitempty"`
}

// ResourceNetworkingInfo is the networking metadata of a resource.
type ResourceNetworkingInfo struct {
	TargetLabels map[string]string     `json:"targetLabels,omitempty"`
	TargetRefs   []ResourceRef         `json:"targetRefs,omitempty"`
	Labels       map[string]string     `json:"labels,omitempty"`
	Ingress      []LoadBalancerIngress `json:"ingress,omitempty"`
	ExternalURLs []string              `json:"externalURLs,omitempty"`
}

// ResourceNode is one node of a resource tree.
type ResourceNode struct {
	ResourceRef
	ParentRefs      []ResourceRef           `json:"parentRefs,omitempty"`
	Info            []InfoItem              `json:"info,omitempty"`
	NetworkingInfo  *ResourceNetworkingInfo `json:"networkingInfo,omitempty"`
	ResourceVersion string                  `json:"resourceVersion,omitempty"`
	Images          []string                `json:"images,omitempty"`
	Health          *v1alpha1.HealthStatus  `json:"health,omitempty"`
	CreatedAt       *metav1.Time            `json:"createdAt,omitempty"`
}

// HostResourceInfo is the capacity and the requests of one resource on a node.
type HostResourceInfo struct {
	ResourceName         corev1.ResourceName `json:"resourceName,omitempty"`
	RequestedByApp       int64               `json:"requestedByApp,omitempty"`
	RequestedByNeighbors int64               `json:"requestedByNeighbors,omitempty"`
	Capacity             int64               `json:"capacity,omitempty"`
}

// HostInfo describes a node running pods of the Application.
type HostInfo struct {
	Name          string                `json:"name,omitempty"`
	ResourcesInfo []HostResourceInfo    `json:"resourcesInfo,omitempty"`
	SystemInfo    corev1.NodeSystemInfo `json:"systemInfo,omitzero"`
	Labels        map[string]string     `json:"labels,omitempty"`
}

// ApplicationTree is the resource tree of an Application: managed resources and their
// descendants, orphaned resources of the destination namespace (when the project monitors
// them) and the nodes running its pods.
type ApplicationTree struct {
	Nodes         []ResourceNode `json:"nodes,omitempty"`
	OrphanedNodes []ResourceNode `json:"orphanedNodes,omitempty"`
	Hosts         []HostInfo     `json:"hosts,omitempty"`
}

// FindNode returns the node (managed or orphaned) with the given identity, or nil.
func (t *ApplicationTree) FindNode(group, kind, namespace, name string) *ResourceNode {
	if t == nil {
		return nil
	}
	for _, list := range [][]ResourceNode{t.Nodes, t.OrphanedNodes} {
		for i := range list {
			n := &list[i]
			if n.Group == group && n.Kind == kind && n.Namespace == namespace && n.Name == name {
				return n
			}
		}
	}
	return nil
}

// ApplicationTreeEvent is one event of GET /api/v1/stream/applications/{name}/resource-tree:
// ADDED with the first tree, MODIFIED with every changed tree, DELETED (empty tree) when
// the Application is deleted.
type ApplicationTreeEvent = WatchEvent[ApplicationTree]

// ApplicationManagedResourcesQuery selects managed resources of an Application; every
// non-empty filter must match exactly. Version is accepted for compatibility and ignored.
type ApplicationManagedResourcesQuery struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	Namespace    string `query:"namespace" json:"-"`
	ResourceName string `query:"resourceName" json:"-"`
	Version      string `query:"version" json:"-"`
	Group        string `query:"group" json:"-"`
	Kind         string `query:"kind" json:"-"`
}

// ResourceDiff is one managed resource with its desired and live states (JSON; core
// Secret data and sensitive annotations masked). "null" stands for a missing side.
type ResourceDiff struct {
	Group               string `json:"group,omitempty"`
	Kind                string `json:"kind,omitempty"`
	Namespace           string `json:"namespace,omitempty"`
	Name                string `json:"name,omitempty"`
	TargetState         string `json:"targetState,omitempty"`
	LiveState           string `json:"liveState,omitempty"`
	Diff                string `json:"diff,omitempty"`
	Hook                bool   `json:"hook,omitempty"`
	NormalizedLiveState string `json:"normalizedLiveState,omitempty"`
	PredictedLiveState  string `json:"predictedLiveState,omitempty"`
	ResourceVersion     string `json:"resourceVersion,omitempty"`
	Modified            bool   `json:"modified,omitempty"`
}

// ManagedResourcesResponse lists managed resources (hooks excluded).
type ManagedResourcesResponse struct {
	Items []ResourceDiff `json:"items"`
}

// ApplicationServerSideDiffRequest asks for a server-side (dry-run apply) diff of
// index-aligned live resources and target manifests: a live state that is empty or "null"
// is a resource to create, an empty or "null" target manifest a resource to prune. Every
// live resource must be a managed resource of the Application.
type ApplicationServerSideDiffRequest struct {
	Name            string         `path:"name" json:"-"`
	AppNamespace    string         `query:"appNamespace" json:"-"`
	Project         string         `query:"project" json:"-"`
	LiveResources   []ResourceDiff `json:"liveResources,omitempty"`
	TargetManifests []string       `json:"targetManifests,omitempty"`
}

// ServerSideDiffResponse is the result of a server-side diff: per item, targetState is
// the predicted live state and liveState the normalized live state.
type ServerSideDiffResponse struct {
	Items    []ResourceDiff `json:"items"`
	Modified bool           `json:"modified"`
}

// ApplicationEventsQuery selects the Events of an Application (no resource given) or of
// one resource of its tree (resourceName/resourceUID and resourceNamespace).
type ApplicationEventsQuery struct {
	Name              string `path:"name" json:"-"`
	AppNamespace      string `query:"appNamespace" json:"-"`
	Project           string `query:"project" json:"-"`
	ResourceNamespace string `query:"resourceNamespace" json:"-"`
	ResourceName      string `query:"resourceName" json:"-"`
	ResourceUID       string `query:"resourceUID" json:"-"`
}

// ApplicationResourceResponse carries a live manifest (JSON; core Secret data masked).
type ApplicationResourceResponse struct {
	Manifest string `json:"manifest"`
}

// ApplicationResourcePatchRequest patches a live resource of an Application's tree.
// PatchType is json, merge (default) or strategic; the media types
// application/json-patch+json, application/merge-patch+json and
// application/strategic-merge-patch+json are accepted too.
type ApplicationResourcePatchRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	ResourceName string `query:"resourceName" json:"-"`
	Namespace    string `query:"namespace" json:"-"`
	Version      string `query:"version" json:"-"`
	Group        string `query:"group" json:"-"`
	Kind         string `query:"kind" json:"-"`
	Patch        string `json:"patch"`
	PatchType    string `json:"patchType,omitempty"`
}

// ApplicationResourceDeleteRequest deletes a live resource of an Application's tree:
// orphan deletes with propagation Orphan, force with Background and a zero grace period,
// otherwise Foreground (orphan wins over force).
type ApplicationResourceDeleteRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	ResourceName string `query:"resourceName" json:"-"`
	Namespace    string `query:"namespace" json:"-"`
	Version      string `query:"version" json:"-"`
	Group        string `query:"group" json:"-"`
	Kind         string `query:"kind" json:"-"`
	Force        *bool  `query:"force" json:"-"`
	Orphan       *bool  `query:"orphan" json:"-"`
}

// ResourceActionParam is an input declared by an action's discovery.
type ResourceActionParam struct {
	Name string `json:"name"`
}

// ResourceAction is an action available for a resource.
type ResourceAction struct {
	Name        string                `json:"name"`
	Params      []ResourceActionParam `json:"params,omitempty"`
	Disabled    bool                  `json:"disabled,omitempty"`
	IconClass   string                `json:"iconClass,omitempty"`
	DisplayName string                `json:"displayName,omitempty"`
}

// ResourceActionsListResponse lists the actions of a resource.
type ResourceActionsListResponse struct {
	Actions []ResourceAction `json:"actions"`
}

// ResourceActionParameter is a value passed to an action (actionParams[name] in Lua).
type ResourceActionParameter struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ResourceActionRunRequest runs an action on a resource of an Application's tree, or on
// the Application itself (group cd.patrol.example, kind Application, resourceName = the
// Application's name).
type ResourceActionRunRequest struct {
	Name                     string                    `path:"name" json:"-"`
	AppNamespace             string                    `query:"appNamespace" json:"-"`
	Project                  string                    `query:"project" json:"-"`
	ResourceName             string                    `query:"resourceName" json:"-"`
	Namespace                string                    `query:"namespace" json:"-"`
	Version                  string                    `query:"version" json:"-"`
	Group                    string                    `query:"group" json:"-"`
	Kind                     string                    `query:"kind" json:"-"`
	Action                   string                    `json:"action"`
	ResourceActionParameters []ResourceActionParameter `json:"resourceActionParameters,omitempty"`
}

// ApplicationLogsQuery selects pod logs of an Application (GET
// /api/v1/applications/{name}/logs). Pods are the Pod nodes of the resource tree that
// match kind/group/resourceName/namespace, directly or through an ancestor (a Deployment
// selects the pods of its ReplicaSets). PodName implies kind=Pod and resourceName=PodName.
type ApplicationLogsQuery struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	Namespace    string `query:"namespace" json:"-"`
	PodName      string `query:"podName" json:"-"`
	Container    string `query:"container" json:"-"`
	SinceSeconds *int64 `query:"sinceSeconds" json:"-"`
	// SinceTime is an RFC 3339 time.
	SinceTime string `query:"sinceTime" json:"-"`
	// UntilTime (RFC 3339, nanoseconds allowed) ends the stream at the first line after it.
	UntilTime string `query:"untilTime" json:"-"`
	TailLines *int64 `query:"tailLines" json:"-"`
	Follow    *bool  `query:"follow" json:"-"`
	// Filter keeps lines containing the text (case-insensitive unless MatchCase); a
	// leading "!" keeps lines that do not contain it.
	Filter       string `query:"filter" json:"-"`
	MatchCase    *bool  `query:"matchCase" json:"-"`
	Previous     *bool  `query:"previous" json:"-"`
	Kind         string `query:"kind" json:"-"`
	Group        string `query:"group" json:"-"`
	ResourceName string `query:"resourceName" json:"-"`
	// Download returns the lines as a text attachment instead of Server-Sent Events.
	Download *bool `query:"download" json:"-"`
}

// ApplicationPodLogsQuery selects the logs of one pod (GET
// /api/v1/applications/{name}/pods/{pod}/logs); see ApplicationLogsQuery.
type ApplicationPodLogsQuery struct {
	Name         string `path:"name" json:"-"`
	Pod          string `path:"pod" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	Namespace    string `query:"namespace" json:"-"`
	Container    string `query:"container" json:"-"`
	SinceSeconds *int64 `query:"sinceSeconds" json:"-"`
	SinceTime    string `query:"sinceTime" json:"-"`
	UntilTime    string `query:"untilTime" json:"-"`
	TailLines    *int64 `query:"tailLines" json:"-"`
	Follow       *bool  `query:"follow" json:"-"`
	Filter       string `query:"filter" json:"-"`
	MatchCase    *bool  `query:"matchCase" json:"-"`
	Previous     *bool  `query:"previous" json:"-"`
	Download     *bool  `query:"download" json:"-"`
}

// LogEntry is one log line. The final entry of a finished stream has Last set and no
// content. Content is untrusted text from the container.
type LogEntry struct {
	Content      string       `json:"content"`
	TimeStamp    *metav1.Time `json:"timeStamp,omitempty"`
	TimeStampStr string       `json:"timeStampStr,omitempty"`
	PodName      string       `json:"podName"`
	Last         bool         `json:"last"`
}

// TerminalRequest addresses a container for the web terminal.
type TerminalRequest struct {
	AppName      string `query:"appName" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	ProjectName  string `query:"projectName" json:"-"`
	Namespace    string `query:"namespace" json:"-"`
	Pod          string `query:"pod" json:"-"`
	Container    string `query:"container" json:"-"`
	// Shell is used when it is in exec.shells; otherwise the configured shells are tried in
	// order.
	Shell string `query:"shell" json:"-"`
}

// Terminal message operations.
const (
	TerminalOpStdin  = "stdin"
	TerminalOpStdout = "stdout"
	TerminalOpResize = "resize"
)

// TerminalMessage is one WebSocket text frame of the web terminal: stdin and resize from
// the client, stdout from the server.
type TerminalMessage struct {
	// Operation is stdin or resize (client) or stdout (server); any other value ends the
	// session.
	Operation string `json:"operation" jsonschema:"enum=stdin,enum=stdout,enum=resize"`
	// Data is the input (stdin) or output (stdout); resize frames omit it.
	Data string `json:"data,omitempty"`
	// Rows is the terminal height in characters (resize).
	Rows uint16 `json:"rows,omitempty"`
	// Cols is the terminal width in characters (resize).
	Cols uint16 `json:"cols,omitempty"`
}

// GetApplicationResourceTree returns the resource tree of an Application.
func (c *Client) GetApplicationResourceTree(ctx context.Context, q *ApplicationTreeQuery) (*ApplicationTree, error) {
	var out ApplicationTree
	return &out, c.Do(ctx, http.MethodGet, PathApplicationResourceTree, q, &out)
}

// WatchApplicationResourceTree opens the resource tree stream; read it with
// Watch[ApplicationTree].
func (c *Client) WatchApplicationResourceTree(ctx context.Context, q *ApplicationTreeQuery, opts *StreamOptions) (*EventStream, error) {
	return c.Stream(ctx, PathStreamApplicationResourceTree, q, opts)
}

// GetApplicationManagedResources returns the managed resources (diff data) of an
// Application.
func (c *Client) GetApplicationManagedResources(ctx context.Context, q *ApplicationManagedResourcesQuery) (*ManagedResourcesResponse, error) {
	var out ManagedResourcesResponse
	return &out, c.Do(ctx, http.MethodGet, PathApplicationManagedResources, q, &out)
}

// ApplicationServerSideDiff runs a server-side diff.
func (c *Client) ApplicationServerSideDiff(ctx context.Context, req *ApplicationServerSideDiffRequest) (*ServerSideDiffResponse, error) {
	var out ServerSideDiffResponse
	return &out, c.Do(ctx, http.MethodPost, PathApplicationServerSideDiff, req, &out)
}

// ListApplicationEvents lists the Kubernetes Events of an Application or of one of its
// resources.
func (c *Client) ListApplicationEvents(ctx context.Context, q *ApplicationEventsQuery) (*corev1.EventList, error) {
	var out corev1.EventList
	return &out, c.Do(ctx, http.MethodGet, PathApplicationEvents, q, &out)
}

// GetApplicationResource returns the live manifest of a resource of an Application.
func (c *Client) GetApplicationResource(ctx context.Context, q *ApplicationResourceRequest) (*ApplicationResourceResponse, error) {
	var out ApplicationResourceResponse
	return &out, c.Do(ctx, http.MethodGet, PathApplicationResource, q, &out)
}

// PatchApplicationResource patches a live resource and returns its new manifest.
func (c *Client) PatchApplicationResource(ctx context.Context, req *ApplicationResourcePatchRequest) (*ApplicationResourceResponse, error) {
	var out ApplicationResourceResponse
	return &out, c.Do(ctx, http.MethodPost, PathApplicationResource, req, &out)
}

// DeleteApplicationResource deletes a live resource of an Application.
func (c *Client) DeleteApplicationResource(ctx context.Context, req *ApplicationResourceDeleteRequest) error {
	return c.Do(ctx, http.MethodDelete, PathApplicationResource, req, nil)
}

// ListApplicationResourceActions lists the actions of a resource (or of the Application
// itself).
func (c *Client) ListApplicationResourceActions(ctx context.Context, q *ApplicationResourceRequest) (*ResourceActionsListResponse, error) {
	var out ResourceActionsListResponse
	return &out, c.Do(ctx, http.MethodGet, PathApplicationResourceActions, q, &out)
}

// RunApplicationResourceAction runs a resource action.
func (c *Client) RunApplicationResourceAction(ctx context.Context, req *ResourceActionRunRequest) error {
	return c.Do(ctx, http.MethodPost, PathApplicationResourceActions, req, nil)
}

// StreamApplicationLogs opens the log stream of an Application; read it with ReadLogs.
func (c *Client) StreamApplicationLogs(ctx context.Context, q *ApplicationLogsQuery) (*EventStream, error) {
	return c.Stream(ctx, PathApplicationLogs, q, nil)
}

// StreamPodLogs opens the log stream of one pod; read it with ReadLogs.
func (c *Client) StreamPodLogs(ctx context.Context, q *ApplicationPodLogsQuery) (*EventStream, error) {
	return c.Stream(ctx, PathApplicationPodLogs, q, nil)
}

// ReadLogs reads log entries from s until the final entry (Last), the end of the stream or
// an error, calling fn for every entry with content (not for the final one). It closes s.
// fn may return ErrStopWatch to stop early.
func ReadLogs(s *EventStream, fn func(LogEntry) error) error {
	defer func() { _ = s.Close() }()
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var e LogEntry
		if err := ev.Decode(&e); err != nil {
			return err
		}
		if e.Last {
			return nil
		}
		if err := fn(e); err != nil {
			if errors.Is(err, ErrStopWatch) {
				return nil
			}
			return err
		}
	}
}

// DownloadApplicationLogs returns the logs of an Application as text (one line per entry);
// the caller closes the body. Download is forced on.
func (c *Client) DownloadApplicationLogs(ctx context.Context, q *ApplicationLogsQuery) (io.ReadCloser, error) {
	cp := *q
	yes := true
	cp.Download = &yes
	resp, err := c.DoRaw(ctx, http.MethodGet, PathApplicationLogs, &cp, nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// DefaultTerminalReadLimit bounds one terminal message received by the client.
const DefaultTerminalReadLimit = 1 << 20

// TerminalSession is an open web terminal.
type TerminalSession struct {
	conn *websocket.Conn
}

// OpenTerminal opens a web terminal session (WebSocket). ctx bounds the handshake and
// must stay alive for the session (canceling it may close the connection). Errors before
// the upgrade are *APIError values.
func (c *Client) OpenTerminal(ctx context.Context, req *TerminalRequest) (*TerminalSession, error) {
	b, err := buildRequest(http.MethodGet, PathTerminal, req)
	if err != nil {
		return nil, err
	}
	u := *c.base
	u.Path = c.base.Path + b.path
	u.RawPath = c.base.Path + b.rawPath
	u.RawQuery = b.query.Encode()
	hdr := c.headers.Clone()
	hdr.Set("User-Agent", c.userAgent)
	if tok := c.Token(); tok != "" {
		hdr.Set("Authorization", "Bearer "+tok)
	}
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient:      c.hc,
		HTTPHeader:      hdr,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			if resp.Body != nil {
				defer func() { _ = resp.Body.Close() }()
			}
			return nil, errorFromResponse(resp)
		}
		return nil, fmt.Errorf("open terminal: %w", err)
	}
	conn.SetReadLimit(DefaultTerminalReadLimit)
	return &TerminalSession{conn: conn}, nil
}

func (t *TerminalSession) send(ctx context.Context, m TerminalMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return t.conn.Write(ctx, websocket.MessageText, b)
}

// SendStdin sends input to the terminal.
func (t *TerminalSession) SendStdin(ctx context.Context, data string) error {
	return t.send(ctx, TerminalMessage{Operation: TerminalOpStdin, Data: data})
}

// Resize sends the terminal size.
func (t *TerminalSession) Resize(ctx context.Context, rows, cols uint16) error {
	return t.send(ctx, TerminalMessage{Operation: TerminalOpResize, Rows: rows, Cols: cols})
}

// Send sends a raw terminal message.
func (t *TerminalSession) Send(ctx context.Context, m TerminalMessage) error { return t.send(ctx, m) }

// Recv returns the next message from the server. io.EOF reports a normal closure.
func (t *TerminalSession) Recv(ctx context.Context) (TerminalMessage, error) {
	typ, b, err := t.conn.Read(ctx)
	if err != nil {
		if s := websocket.CloseStatus(err); s == websocket.StatusNormalClosure || s == websocket.StatusGoingAway {
			return TerminalMessage{}, io.EOF
		}
		return TerminalMessage{}, err
	}
	if typ != websocket.MessageText {
		return TerminalMessage{}, errors.New("terminal: unexpected binary message")
	}
	var m TerminalMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return TerminalMessage{}, fmt.Errorf("terminal: decode message: %w", err)
	}
	return m, nil
}

// ReadUntil reads stdout messages until their concatenation contains want, returning the
// output read so far. It is a convenience for scripts and tests.
func (t *TerminalSession) ReadUntil(ctx context.Context, want string) (string, error) {
	var sb strings.Builder
	for {
		m, err := t.Recv(ctx)
		if err != nil {
			return sb.String(), err
		}
		if m.Operation == TerminalOpStdout {
			sb.WriteString(m.Data)
			if strings.Contains(sb.String(), want) {
				return sb.String(), nil
			}
		}
	}
}

// CloseStatus returns the WebSocket close code and reason of err (from Recv), or -1.
func CloseStatus(err error) (code int, reason string) {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return int(ce.Code), ce.Reason
	}
	return -1, ""
}

// Close closes the session.
func (t *TerminalSession) Close() error {
	return t.conn.Close(websocket.StatusNormalClosure, "")
}
