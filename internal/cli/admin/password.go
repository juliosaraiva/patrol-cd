package admin

import (
	"fmt"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// initialPasswordKey is the data key of the initial admin password Secret.
const initialPasswordKey = "password"

func newInitialPasswordCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "initial-password",
		Short: "Prints initial password to log in to Patrol CD for the first time",
		Long: fmt.Sprintf(`Prints the password generated for the admin account at installation, stored in Secret %s
(key %q) of the control-plane namespace. Change it after the first login.`, initialAdminSecretName, initialPasswordKey),
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			kc, err := kube.get(nil)
			if err != nil {
				return err
			}
			sec, err := kc.kube.CoreV1().Secrets(kc.namespace).Get(c.Context(), initialAdminSecretName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if pass, ok := sec.Data[initialPasswordKey]; ok {
				// Printing the password is the purpose of this command.
				_, _ = fmt.Fprintln(f.out(), string(pass))
				_, _ = fmt.Fprintln(f.out(), "\n This password must be only used for first time login. We strongly recommend you update the password using `patrol account update-password`.")
			}
			return nil
		},
	}
}

// newRedisInitialPasswordCommand keeps upstream's command for scripts: Patrol CD has no Redis
// (ADR-0004), so there is no password to create.
func newRedisInitialPasswordCommand(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "redis-initial-password",
		Short: "Not applicable: Patrol CD does not use Redis (kept for compatibility; does nothing)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_, _ = fmt.Fprintln(f.out(), "Patrol CD does not use Redis (ADR-0004): there is no Redis password to create. Nothing to do.")
			return nil
		},
	}
}
