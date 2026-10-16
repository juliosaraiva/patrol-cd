package cmdutil

import (
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/tools/clientcmd"
)

// kubectlFlags are the kubectl client flags accepted by the command groups and their
// subcommands (upstream initialize.InitCommand adds them to the groups only); they take
// effect in core mode and with --port-forward.
var kubectlFlags = map[string]bool{
	"cluster": true, "context": true, "insecure-skip-tls-verify": true, "namespace": true,
	"password": true, "proxy-url": true, "request-timeout": true, "token": true, "user": true,
	"username": true,
}

// AddKubectlFlags adds the kubectl client flags (--kubeconfig, --context, --cluster,
// --user, -n/--namespace, --token, --username, --password, --proxy-url,
// --request-timeout, --insecure-skip-tls-verify) to cmd as persistent flags, bound to
// f.KubeOverrides and f.Kubeconfig, so that every subcommand accepts them (`patrol --core
// app list -n patrol-staging`). As upstream, they are only used in core mode (and by
// --port-forward). A subcommand's own flag of the same name (cluster add/set --namespace,
// cluster add/rm --kubeconfig, repo add --username/--password/--proxy-url) takes
// precedence on that subcommand.
func AddKubectlFlags(cmd *cobra.Command, f *Factory) *cobra.Command {
	tmp := pflag.NewFlagSet("kubectl", pflag.ContinueOnError)
	clientcmd.BindOverrideFlags(&f.KubeOverrides, tmp, clientcmd.RecommendedConfigOverrideFlags(""))
	tmp.VisitAll(func(fl *pflag.Flag) {
		if kubectlFlags[fl.Name] && cmd.Flags().Lookup(fl.Name) == nil && cmd.PersistentFlags().Lookup(fl.Name) == nil {
			cmd.PersistentFlags().AddFlag(fl)
		}
	})
	if cmd.Flags().Lookup("kubeconfig") == nil && cmd.PersistentFlags().Lookup("kubeconfig") == nil {
		cmd.PersistentFlags().StringVar(&f.Kubeconfig, "kubeconfig", "", "Path to a kube config. Only required if out-of-cluster")
	}
	return cmd
}

// KubeconfigPaths returns the kubeconfig files to load: explicit, else KUBECONFIG, else
// $HOME/.kube/config, all read from the factory's environment (so that a command never
// falls back to the process owner's kubeconfig when its environment says otherwise).
func (f *Factory) KubeconfigPaths(explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	if v := f.Getenv(EnvKubeconfig); v != "" {
		return filepath.SplitList(v)
	}
	home := f.Getenv("HOME")
	if home == "" && runtime.GOOS == "windows" {
		home = f.Getenv("USERPROFILE")
	}
	if home == "" {
		return []string{clientcmd.RecommendedHomeFile}
	}
	return []string{filepath.Join(home, clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)}
}
