package clusters

import (
	"maps"
	"net/url"
	"slices"
	"strings"
	"unicode"

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Input limits (the CRD enforces the same bounds; checking them here gives 400s with
// field names instead of admission errors).
const (
	maxServerLength = 2048
	maxNameLength   = 253
	maxNamespaces   = 1000
)

// lastAppliedAnnotation may not be set through the API (upstream REPO-083).
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// desired is a validated cluster write: the Cluster spec, the object labels and
// annotations, and the credentials Secret data.
type desired struct {
	spec        v1alpha1.ClusterSpec
	labels      map[string]string
	annotations map[string]string
	creds       map[string][]byte
}

// normalizeServer validates a server URL: http or https, a host, no credentials, query or
// fragment, not a link-local or metadata address (unless allowed). It returns the URL
// without trailing slashes.
func normalizeServer(server string, allowLinkLocal bool) (string, error) {
	if server == "" {
		return "", rest.InvalidArgument("server is required")
	}
	if len(server) > maxServerLength {
		return "", rest.InvalidArgument("server must be at most %d characters", maxServerLength)
	}
	if strings.IndexFunc(server, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", rest.InvalidArgument("server must not contain whitespace or control characters")
	}
	u, err := url.ParseRequestURI(server)
	if err != nil {
		return "", rest.InvalidArgument("server %q is not a valid URL", server)
	}
	if s := strings.ToLower(u.Scheme); s != "https" && s != "http" {
		return "", rest.InvalidArgument("server must be an http(s) URL, not %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", rest.InvalidArgument("server %q has no host", server)
	}
	if u.User != nil {
		return "", rest.InvalidArgument("server must not contain credentials; use config.username/config.password")
	}
	if u.RawQuery != "" || u.Fragment != "" || strings.Contains(server, "#") {
		return "", rest.InvalidArgument("server must not contain a query or fragment")
	}
	if err := checkURLHost(u, allowLinkLocal); err != nil {
		return "", rest.InvalidArgument("server: %v", err)
	}
	return clusterlib.NormalizeServer(server), nil
}

// validateInput validates a create/update request body and converts it. server is the
// already validated server URL.
func validateInput(in *apiclient.ClusterInput, server string, allowLinkLocal bool) (*desired, error) {
	d := &desired{}
	name := in.Name
	if name == "" {
		name = server // upstream: the name defaults to the server URL
	}
	if len(name) > maxNameLength {
		return nil, rest.InvalidArgument("name must be at most %d characters", maxNameLength)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return nil, rest.InvalidArgument("name must not contain control characters")
	}
	namespaces := clusterlib.NormalizeNamespaces(in.Namespaces)
	if len(namespaces) > maxNamespaces {
		return nil, rest.InvalidArgument("at most %d namespaces are allowed", maxNamespaces)
	}
	seen := map[string]bool{}
	for _, ns := range namespaces {
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return nil, rest.InvalidArgument("namespace %q is invalid: %s", ns, strings.Join(errs, "; "))
		}
		if seen[ns] {
			return nil, rest.InvalidArgument("namespace %q is listed twice", ns)
		}
		seen[ns] = true
	}
	if in.Project != "" {
		if errs := validation.IsDNS1123Subdomain(in.Project); len(errs) > 0 {
			return nil, rest.InvalidArgument("project %q is invalid: %s", in.Project, strings.Join(errs, "; "))
		}
	}
	var shard *int64
	if in.Shard != nil && *in.Shard >= 0 { // upstream CLI: -1 means unset
		v := *in.Shard
		shard = &v
	}
	if err := validateMetadata(in.Labels, in.Annotations); err != nil {
		return nil, err
	}
	cfg, err := validateConfig(&in.Config, allowLinkLocal)
	if err != nil {
		return nil, err
	}
	d.spec = v1alpha1.ClusterSpec{
		Server:           server,
		Name:             name,
		Namespaces:       namespaces,
		ClusterResources: in.ClusterResources,
		Project:          in.Project,
		Shard:            shard,
		Config:           *cfg,
	}
	d.labels = maps.Clone(in.Labels)
	d.annotations = maps.Clone(in.Annotations)
	d.creds = credentialsOf(&in.Config)
	return d, nil
}

func validateMetadata(labels, annotations map[string]string) error {
	if _, ok := annotations[lastAppliedAnnotation]; ok {
		return rest.InvalidArgument("annotation %s cannot be set", lastAppliedAnnotation)
	}
	if errs := metav1validation.ValidateLabels(labels, field.NewPath("labels")); len(errs) > 0 {
		return rest.InvalidArgument("%s", errs.ToAggregate().Error())
	}
	if errs := apivalidation.ValidateAnnotations(annotations, field.NewPath("annotations")); len(errs) > 0 {
		return rest.InvalidArgument("%s", errs.ToAggregate().Error())
	}
	return nil
}

// validateConfig checks the non-secret connection settings and converts them to the CRD
// form (credentials go to the Secret).
func validateConfig(c *apiclient.ClusterConfigInput, allowLinkLocal bool) (*v1alpha1.ClusterConfig, error) {
	out := &v1alpha1.ClusterConfig{
		TLSClientConfig:    v1alpha1.TLSClientConfig{Insecure: c.TLSClientConfig.Insecure, ServerName: c.TLSClientConfig.ServerName},
		DisableCompression: c.DisableCompression,
	}
	if c.ProxyURL != "" {
		u, err := clusterlib.ParseProxyURL(c.ProxyURL)
		if err != nil {
			return nil, rest.InvalidArgument("invalid proxyUrl: %v", err)
		}
		if u.User != nil {
			return nil, rest.InvalidArgument("proxyUrl must not contain credentials")
		}
		if u.Hostname() == "" {
			return nil, rest.InvalidArgument("proxyUrl has no host")
		}
		if err := checkURLHost(u, allowLinkLocal); err != nil {
			return nil, rest.InvalidArgument("proxyUrl: %v", err)
		}
		out.ProxyURL = c.ProxyURL
	}
	if (len(c.TLSClientConfig.CertData) > 0) != (len(c.TLSClientConfig.KeyData) > 0) {
		return nil, rest.InvalidArgument("tlsClientConfig.certData and tlsClientConfig.keyData must be set together")
	}
	if a := c.AWSAuthConfig; a != nil {
		cp := *a
		out.AWSAuthConfig = &cp
	}
	if e := c.ExecProviderConfig; e != nil {
		if strings.TrimSpace(e.Command) == "" {
			return nil, rest.InvalidArgument("execProviderConfig.command is required")
		}
		out.ExecProviderConfig = e.DeepCopy()
	}
	return out, nil
}

// credentialsOf returns the credentials Secret data of a request (empty fields omitted).
func credentialsOf(c *apiclient.ClusterConfigInput) map[string][]byte {
	return clusterlib.Credentials{
		BearerToken: c.BearerToken,
		Username:    c.Username,
		Password:    c.Password,
		CAData:      c.TLSClientConfig.CAData,
		CertData:    c.TLSClientConfig.CertData,
		KeyData:     c.TLSClientConfig.KeyData,
	}.SecretData()
}

// parseUpdatedFields validates ?updatedFields=. Unknown paths are ignored, as upstream.
func parseUpdatedFields(values []string) map[string]bool {
	out := map[string]bool{}
	for _, f := range apiclient.SplitUpdatedFields(values) {
		if slices.Contains(apiclient.ClusterUpdatableFields, f) {
			out[f] = true
		}
	}
	return out
}

// usesAmbientAuth reports whether a cluster authenticates through execProviderConfig or
// awsAuthConfig: the Patrol component that connects then runs a command, or obtains a
// token with its own (cloud) identity, instead of using credentials supplied with the
// cluster. The in-cluster server ignores both settings (internal/clusters.RawRestConfig).
func usesAmbientAuth(spec *v1alpha1.ClusterSpec) bool {
	if clusterlib.IsInClusterServer(spec.Server) {
		return false
	}
	return spec.Config.AWSAuthConfig != nil || spec.Config.ExecProviderConfig != nil
}

// rbacObject is the RBAC object of a cluster.
func rbacObject(spec *v1alpha1.ClusterSpec) string {
	return apiclient.ClusterRBACObject(spec.Project, spec.Server)
}
