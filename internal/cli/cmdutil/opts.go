package cmdutil

import (
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Environment variables of the CLI.
const (
	// EnvOpts holds default values of the global flags ("--server x --plaintext").
	EnvOpts = "PATROL_OPTS"
	// EnvServer is the default --server.
	EnvServer = "PATROL_SERVER"
	// EnvAuthToken is the default --auth-token.
	EnvAuthToken = "PATROL_AUTH_TOKEN"
	// EnvCore is the default --core ("true"/"false"; PATROL_OPTS wins when it sets --core).
	EnvCore = "PATROL_CORE"
	// EnvServerName, EnvControllerName and EnvRepoServerName are the default component
	// names (app.kubernetes.io/name labels) used by port-forward and core mode.
	EnvServerName     = "PATROL_SERVER_NAME"
	EnvControllerName = "PATROL_APPLICATION_CONTROLLER_NAME"
	EnvRepoServerName = "PATROL_REPO_SERVER_NAME"
	// EnvKubeconfig lists the kubeconfig files (KUBECONFIG).
	EnvKubeconfig = "KUBECONFIG"

	// Migration aliases of the Argo CD CLI, honored only when the Patrol variable is unset
	// (with a warning); see docs/architecture/components/cli.md.
	EnvArgoOpts      = "ARGOCD_OPTS"
	EnvArgoServer    = "ARGOCD_SERVER"
	EnvArgoAuthToken = "ARGOCD_AUTH_TOKEN"
)

// Default component names.
const (
	DefaultServerName     = "patrol-server"
	DefaultControllerName = "patrol-application-controller"
	DefaultRepoServerName = "patrol-repo-server"
)

// maxOptsBytes bounds PATROL_OPTS.
const maxOptsBytes = 64 << 10

// Opts are the global flag defaults parsed from PATROL_OPTS (upstream ARGOCD_OPTS):
// shell-quoted "--flag value", "--flag" (true) and "--flag=value" tokens.
type Opts map[string]string

// ParseOpts parses s. A token that is not a flag and has no pending flag is an error
// ("PATROL_OPTS invalid at '<x>'").
func ParseOpts(name, s string) (Opts, error) {
	if len(s) > maxOptsBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, maxOptsBytes)
	}
	words, err := shellSplit(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	out := Opts{}
	key := ""
	for _, w := range words {
		switch {
		case strings.HasPrefix(w, "--"):
			if key != "" {
				out[key] = "true"
			}
			key = strings.TrimPrefix(w, "--")
		case key != "":
			out[key] = w
			key = ""
		default:
			return nil, fmt.Errorf("%s invalid at '%s'", name, w)
		}
	}
	if key != "" {
		out[key] = "true"
	}
	for k, v := range out {
		if strings.Contains(k, "=") && v == "true" {
			ak, av, _ := strings.Cut(k, "=")
			if _, ok := out[ak]; !ok {
				out[ak] = av
			}
			delete(out, k)
		}
	}
	return out, nil
}

// String returns the value of key or fallback.
func (o Opts) String(key, fallback string) string {
	if v, ok := o[key]; ok {
		return v
	}
	return fallback
}

// Bool returns whether key is "true".
func (o Opts) Bool(key string) bool { return o.String(key, "false") == "true" }

// Int returns the integer value of key or fallback.
func (o Opts) Int(key string, fallback int) (int, error) {
	v, ok := o[key]
	if !ok {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback, fmt.Errorf("%s: invalid value %q for --%s: %w", EnvOpts, v, key, err)
	}
	return n, nil
}

// StringSlice returns the comma-separated (CSV) values of key or fallback.
func (o Opts) StringSlice(key string, fallback []string) ([]string, error) {
	v, ok := o[key]
	if !ok {
		return fallback, nil
	}
	if v == "" {
		return []string{}, nil
	}
	rec, err := csv.NewReader(strings.NewReader(v)).Read()
	if err != nil {
		return fallback, fmt.Errorf("%s: invalid value for --%s: %w", EnvOpts, key, err)
	}
	return rec, nil
}

// shellSplit splits s into words with POSIX shell quoting rules (single quotes, double
// quotes with backslash escapes, backslash escapes outside quotes).
func shellSplit(s string) ([]string, error) {
	var (
		words []string
		cur   strings.Builder
		in    bool // a word is in progress
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '\n':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		case '\\':
			in = true
			if i+1 < len(s) {
				i++
				if s[i] != '\n' {
					cur.WriteByte(s[i])
				}
			}
		case '\'':
			in = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated single-quoted string")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case '"':
			in = true
			i++
			for ; ; i++ {
				if i >= len(s) {
					return nil, errors.New("unterminated double-quoted string")
				}
				if s[i] == '"' {
					break
				}
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
					i++
					if s[i] != '\n' {
						cur.WriteByte(s[i])
					}
					continue
				}
				cur.WriteByte(s[i])
			}
		default:
			in = true
			cur.WriteByte(c)
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words, nil
}
