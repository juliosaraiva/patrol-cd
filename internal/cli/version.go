package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// ServerComponentName labels the server version.
const ServerComponentName = "patrol-server"

// NewVersionCommand returns `patrol version` (API-061).
func NewVersionCommand(f *cmdutil.Factory) *cobra.Command {
	var (
		short  bool
		client bool
		format string
	)
	c := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Example: `  # Print the full version of client and server to stdout
  patrol version

  # Print only full version of the client - no connection to server will be made
  patrol version --client

  # Print the full version of client and server in JSON format
  patrol version -o json

  # Print only client and server core version strings in YAML format
  patrol version --short -o yaml
`,
		Args: cmdutil.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cv := f.Version
			switch format {
			case output.YAML, output.JSON:
				v := map[string]any{}
				if short {
					v["client"] = map[string]string{CommandName: cv.Version}
				} else {
					v["client"] = cv
				}
				if !client {
					sv, err := serverVersion(c, f)
					if err != nil {
						return err
					}
					if short {
						v["server"] = map[string]string{ServerComponentName: sv.Version}
					} else {
						v["server"] = serverVersionOutput(sv)
					}
				}
				return output.PrintResource(f.Out(), v, format)
			case output.Wide, output.Short, "":
				s := short || format == output.Short
				printClientVersion(f.Out(), cv, s)
				if !client {
					sv, err := serverVersion(c, f)
					if err != nil {
						return err
					}
					printServerVersion(f.Out(), sv, s)
				}
				return nil
			}
			return cmdutil.Fatalf("unknown output format: %s", format)
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide|short")
	c.Flags().BoolVar(&short, "short", false, "print just the version number")
	c.Flags().BoolVar(&client, "client", false, "client version only (no server required)")
	return c
}

func serverVersion(c *cobra.Command, f *cmdutil.Factory) (*apiclient.Version, error) {
	cl, err := f.NewClient(c.Context())
	if err != nil {
		return nil, err
	}
	return cl.Version(c.Context())
}

func printClientVersion(w io.Writer, v cmdutil.ClientVersion, short bool) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", CommandName, v)
	if !short {
		fmt.Fprintf(&b, "  BuildDate: %s\n", v.BuildDate)
		fmt.Fprintf(&b, "  GitCommit: %s\n", v.GitCommit)
		fmt.Fprintf(&b, "  GitTreeState: %s\n", v.GitTreeState)
		if v.GitTag != "" {
			fmt.Fprintf(&b, "  GitTag: %s\n", v.GitTag)
		}
		fmt.Fprintf(&b, "  GoVersion: %s\n", v.GoVersion)
		fmt.Fprintf(&b, "  Compiler: %s\n", v.Compiler)
		fmt.Fprintf(&b, "  Platform: %s\n", v.Platform)
		if v.ExtraBuildInfo != "" {
			fmt.Fprintf(&b, "  ExtraBuildInfo: %s\n", v.ExtraBuildInfo)
		}
	}
	_, _ = io.WriteString(w, b.String())
}

func printServerVersion(w io.Writer, v *apiclient.Version, short bool) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", ServerComponentName, output.Sanitize(v.Version))
	if !short {
		for _, kv := range [][2]string{
			{"BuildDate", v.BuildDate}, {"GitCommit", v.GitCommit}, {"GitTreeState", v.GitTreeState},
			{"GitTag", v.GitTag}, {"GoVersion", v.GoVersion}, {"Compiler", v.Compiler}, {"Platform", v.Platform},
			{"Kustomize Version", v.KustomizeVersion}, {"Helm Version", v.HelmVersion},
			{"Kubectl Version", v.KubectlVersion}, {"Jsonnet Version", v.JsonnetVersion},
		} {
			if kv[1] != "" {
				fmt.Fprintf(&b, "  %s: %s\n", kv[0], output.Sanitize(kv[1]))
			}
		}
		for _, k := range sortedKeys(v.ExtraBuildInfo) {
			fmt.Fprintf(&b, "  %s: %s\n", output.Sanitize(k), output.Sanitize(v.ExtraBuildInfo[k]))
		}
	}
	_, _ = io.WriteString(w, b.String())
}

// serverVersionOut is the server part of `version -o json|yaml` with the upstream field
// names (the API itself uses camelCase).
type serverVersionOut struct {
	Version          string            `json:"Version"`
	BuildDate        string            `json:"BuildDate"`
	GitCommit        string            `json:"GitCommit"`
	GitTag           string            `json:"GitTag"`
	GitTreeState     string            `json:"GitTreeState"`
	GoVersion        string            `json:"GoVersion"`
	Compiler         string            `json:"Compiler"`
	Platform         string            `json:"Platform"`
	KustomizeVersion string            `json:"KustomizeVersion,omitempty"`
	HelmVersion      string            `json:"HelmVersion,omitempty"`
	KubectlVersion   string            `json:"KubectlVersion,omitempty"`
	JsonnetVersion   string            `json:"JsonnetVersion,omitempty"`
	ExtraBuildInfo   map[string]string `json:"ExtraBuildInfo,omitempty"`
}

func serverVersionOutput(v *apiclient.Version) serverVersionOut {
	return serverVersionOut{
		Version: v.Version, BuildDate: v.BuildDate, GitCommit: v.GitCommit, GitTag: v.GitTag,
		GitTreeState: v.GitTreeState, GoVersion: v.GoVersion, Compiler: v.Compiler, Platform: v.Platform,
		KustomizeVersion: v.KustomizeVersion, HelmVersion: v.HelmVersion, KubectlVersion: v.KubectlVersion,
		JsonnetVersion: v.JsonnetVersion, ExtraBuildInfo: v.ExtraBuildInfo,
	}
}
