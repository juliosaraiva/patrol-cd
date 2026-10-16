package generators

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Plugin generator defaults.
const (
	// DefaultPluginRequestTimeout is the request timeout when the ConfigMap sets none.
	DefaultPluginRequestTimeout = 30 * time.Second
	// DefaultPluginMaxResponseBytes caps a plugin response body.
	DefaultPluginMaxResponseBytes = 16 << 20
	// PluginUserAgent is sent with every plugin request.
	PluginUserAgent = "patrol-applicationset"
	// DefaultSettingsSecretName is the Secret "$<key>" plugin tokens are read from.
	DefaultSettingsSecretName = "patrol-secret"
)

// PluginConfig configures the Plugin generator.
type PluginConfig struct {
	// HTTPClient is the base client (nil: a client with the default transport). Its Timeout is
	// replaced by the ConfigMap's requestTimeout.
	HTTPClient *http.Client
	// MaxResponseBytes caps responses (0: DefaultPluginMaxResponseBytes).
	MaxResponseBytes int64
	// SettingsSecretName is the Secret "$<key>" token references read (default patrol-secret).
	SettingsSecretName string
}

// PluginGenerator obtains parameter sets from an HTTP plugin.
type PluginGenerator struct {
	cfg Config
}

// RequeueAfter implements Generator: requeueAfterSeconds, else 30 minutes.
func (g *PluginGenerator) RequeueAfter(gen *v1alpha1.ApplicationSetGenerator) time.Duration {
	return requeueSeconds(gen.Plugin.RequeueAfterSeconds, DefaultPluginRequeueAfter)
}

// Template implements Generator.
func (g *PluginGenerator) Template(gen *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return &gen.Plugin.Template
}

// PluginRequest is the body POSTed to <baseUrl>/api/v1/getparams.execute.
type PluginRequest struct {
	ApplicationSetName string               `json:"applicationSetName"`
	Input              v1alpha1.PluginInput `json:"input"`
}

// PluginResponse is the expected response body.
type PluginResponse struct {
	Output struct {
		Parameters []map[string]any `json:"parameters"`
	} `json:"output"`
}

// GenerateParams implements Generator. Output objects are kept as returned (goTemplate) or
// flattened with dot keys (fasttemplate); every parameter set gets generator.input.parameters
// (the plugin input) and values.
func (g *PluginGenerator) GenerateParams(ctx context.Context, gen *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	if gen == nil || gen.Plugin == nil {
		return nil, ErrEmptyAppSetGenerator
	}
	p := gen.Plugin
	call, err := g.client(ctx, p.ConfigMapRef.Name)
	if err != nil {
		return nil, fmt.Errorf("error getting plugin from generator: %w", err)
	}
	out, err := call.list(ctx, appSet.Name, p.Input.Parameters)
	if err != nil {
		return nil, fmt.Errorf("error listing params: %w", err)
	}
	opts := TemplateOptions(appSet)
	res := make([]map[string]any, 0, len(out))
	for _, obj := range out {
		params := map[string]any{}
		if opts.GoTemplate {
			maps.Copy(params, obj)
		} else {
			maps.Copy(params, flattenObject(obj))
		}
		params["generator"] = map[string]any{
			"input": map[string]any{"parameters": pluginParameters(p.Input.Parameters)},
		}
		if err := appendTemplatedValues(p.Values, params, opts); err != nil {
			return nil, fmt.Errorf("error generating params: %w", err)
		}
		res = append(res, params)
	}
	return res, nil
}

// pluginParameters decodes the input parameters into generic values for templates.
func pluginParameters(in v1alpha1.PluginParameters) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		var decoded any
		if err := json.Unmarshal(v.Raw, &decoded); err != nil {
			decoded = string(v.Raw)
		}
		out[k] = decoded
	}
	return out
}

type pluginCall struct {
	baseURL  string
	token    string
	client   *http.Client
	maxBytes int64
}

func (g *PluginGenerator) client(ctx context.Context, name string) (*pluginCall, error) {
	cm := &corev1.ConfigMap{}
	if err := g.cfg.Client.Get(ctx, client.ObjectKey{Namespace: g.cfg.Namespace, Name: name}, cm); err != nil {
		return nil, fmt.Errorf("error fetching ConfigMap: %w", err)
	}
	baseURL := cm.Data["baseUrl"]
	if baseURL == "" {
		return nil, fmt.Errorf("error fetching ConfigMap: %w", errors.New("baseUrl not found in ConfigMap"))
	}
	tokenRef := cm.Data["token"]
	if tokenRef == "" {
		return nil, fmt.Errorf("error fetching ConfigMap: %w", errors.New("token not found in ConfigMap"))
	}
	token, err := g.token(ctx, tokenRef)
	if err != nil {
		return nil, fmt.Errorf("error fetching Secret token: %w", err)
	}
	timeout := DefaultPluginRequestTimeout
	if raw, ok := cm.Data["requestTimeout"]; ok {
		secs, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("error set requestTimeout : %w", err)
		}
		if secs > 0 {
			timeout = time.Duration(secs) * time.Second
		}
	}
	base := g.cfg.Plugin.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	hc := *base
	hc.Timeout = timeout
	maxBytes := g.cfg.Plugin.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = DefaultPluginMaxResponseBytes
	}
	return &pluginCall{baseURL: baseURL, token: token, client: &hc, maxBytes: maxBytes}, nil
}

// token resolves "$<key>" (key of the settings Secret) or "$<secret>:<key>" in the control-plane
// namespace. A key absent from an existing Secret leaves the reference literal (upstream).
func (g *PluginGenerator) token(ctx context.Context, ref string) (string, error) {
	if !strings.HasPrefix(ref, "$") {
		return "", fmt.Errorf("token is empty, or does not reference a secret key starting with '$': %v", ref)
	}
	secretName := g.cfg.Plugin.SettingsSecretName
	if secretName == "" {
		secretName = DefaultSettingsSecretName
	}
	key := ref[1:]
	if name, k, ok := strings.Cut(ref[1:], ":"); ok {
		secretName, key = name, k
	}
	secret := &corev1.Secret{}
	if err := g.cfg.Client.Get(ctx, client.ObjectKey{Namespace: g.cfg.Namespace, Name: secretName}, secret); err != nil {
		return "", fmt.Errorf("error fetching secret %s/%s: %w", g.cfg.Namespace, secretName, err)
	}
	value, ok := secret.Data[key]
	if !ok {
		g.cfg.logger().WarnContext(ctx, "plugin token key not found in secret; the reference is used literally",
			"secret", g.cfg.Namespace+"/"+secretName, "key", key)
		return ref, nil
	}
	return strings.TrimSpace(string(value)), nil
}

func (c *pluginCall) list(ctx context.Context, appSetName string, params v1alpha1.PluginParameters) ([]map[string]any, error) {
	wrap := func(err error) error { return fmt.Errorf("error get api '%s': %w", appSetName, err) }
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(PluginRequest{ApplicationSetName: appSetName, Input: v1alpha1.PluginInput{Parameters: params}}); err != nil {
		return nil, fmt.Errorf("NewRequest returned unexpected error: %w", err)
	}
	base := c.baseURL
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"api/v1/getparams.execute", &body)
	if err != nil {
		return nil, fmt.Errorf("NewRequest returned unexpected error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", PluginUserAgent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, wrap(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return nil, wrap(err)
	}
	if int64(len(data)) > c.maxBytes {
		return nil, wrap(fmt.Errorf("plugin response exceeds %d bytes", c.maxBytes))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, wrap(apiError(resp.StatusCode, data))
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var out PluginResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, wrap(fmt.Errorf("%s: %s", err.Error(), string(data)))
	}
	return out.Output.Parameters, nil
}

func apiError(code int, data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("API error with status code %d: %s", code, string(data))
	}
	msg := ""
	if v, ok := raw["message"].(string); ok {
		msg = v
	} else if v, ok := raw["error"].(string); ok {
		msg = v
	}
	return fmt.Errorf("API error with status code %d: %s", code, msg)
}
