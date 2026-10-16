package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func newRBACCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rbac",
		Short: "Validate and test RBAC configuration",
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newRBACCanCommand(f, kube), newRBACValidateCommand(f, kube))
	return cmd
}

// rbacPolicy is the user policy of patrol-rbac-cm (or of a local file).
type rbacPolicy struct {
	sources     []rbac.Source
	defaultRole string
	matchMode   string
}

func (p rbacPolicy) empty() bool {
	for _, s := range p.sources {
		if strings.TrimSpace(s.CSV) != "" {
			return false
		}
	}
	return true
}

func policyFromConfigMapData(data map[string]string) rbacPolicy {
	return rbacPolicy{
		sources:     rbac.PolicySources(data),
		defaultRole: data[rbac.ConfigMapPolicyDefaultKey],
		matchMode:   data[rbac.ConfigMapMatchModeKey],
	}
}

// policyFromFile loads a policy file: a ConfigMap manifest with patrol-rbac-cm keys
// (policy.csv, policy.<name>.csv, policy.default, policy.matchMode) or plain CSV.
func policyFromFile(path string) (rbacPolicy, error) {
	data, err := readFileLimited(path)
	if err != nil {
		return rbacPolicy{}, fmt.Errorf("error opening policy file: %w", err)
	}
	var probe map[string]any
	if yaml.Unmarshal(data, &probe) == nil && probe != nil {
		if _, hasData := probe["data"]; hasData || probe["kind"] == "ConfigMap" {
			cm := &corev1.ConfigMap{}
			if err := yaml.Unmarshal(data, cm); err != nil {
				return rbacPolicy{}, fmt.Errorf("error parsing policy ConfigMap: %w", err)
			}
			return policyFromConfigMapData(cm.Data), nil
		}
	}
	return rbacPolicy{sources: []rbac.Source{{Name: path, CSV: string(data)}}}, nil
}

// loadPolicy reads the policy from --policy-file or from patrol-rbac-cm in the namespace.
func loadPolicy(ctx context.Context, kube *kubeOptions, policyFile string) (rbacPolicy, error) {
	if policyFile != "" {
		return policyFromFile(policyFile)
	}
	kc, err := kube.get(nil)
	if err != nil {
		return rbacPolicy{}, err
	}
	cm, err := kc.kube.CoreV1().ConfigMaps(kc.namespace).Get(ctx, settings.DefaultRBACConfigMapName, metav1.GetOptions{})
	if err != nil {
		return rbacPolicy{}, fmt.Errorf("could not get configmap: %w", err)
	}
	return policyFromConfigMapData(cm.Data), nil
}

// exactlyOnePolicySource enforces upstream's "exactly one of --policy-file or --namespace".
func exactlyOnePolicySource(kube *kubeOptions, policyFile string) error {
	if kube.namespaceExplicit() == (policyFile != "") {
		return errors.New("please provide exactly one of --policy-file or --namespace")
	}
	return nil
}

func newRBACCanCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var (
		policyFile  string
		defaultRole string
		useBuiltin  bool
		strict      bool
		quiet       bool
	)
	cmd := &cobra.Command{
		Use:   "can ROLE/SUBJECT ACTION RESOURCE [SUB-RESOURCE]",
		Short: "Check RBAC permissions for a role or subject",
		Long: `
Check whether a given role or subject has appropriate RBAC permissions to do
something.
`,
		Example: `
# Check whether role some:role has permissions to create an application in the
# 'default' project, using a local policy.csv file
patrol admin settings rbac can some:role create application 'default/app' --policy-file policy.csv

# Policy file can also be K8s config map with data keys like patrol-rbac-cm,
# i.e. 'policy.csv' and (optionally) 'policy.default'
patrol admin settings rbac can some:role create application 'default/app' --policy-file patrol-rbac-cm.yaml

# If --policy-file is not given, the ConfigMap 'patrol-rbac-cm' from K8s is
# used. You need to specify the patrol namespace, and make sure that your
# current Kubernetes context is pointing to the cluster Patrol CD is running in
patrol admin settings rbac can some:role create application 'default/app' --namespace patrol

# You can override a possibly configured default role
patrol admin settings rbac can someuser create application 'default/app' --default-role role:readonly
`,
		Args: cobra.RangeArgs(3, 4),
		RunE: func(c *cobra.Command, args []string) error {
			if err := exactlyOnePolicySource(kube, policyFile); err != nil {
				return err
			}
			pol, err := loadPolicy(c.Context(), kube, policyFile)
			if err != nil {
				return err
			}
			if defaultRole == "" {
				defaultRole = pol.defaultRole
			}
			builtin := ""
			if useBuiltin {
				builtin = rbac.BuiltinPolicy()
			}
			enf := rbac.NewEnforcer(rbac.WithLogger(f.logger()))
			if err := enf.SetPolicySources(builtin, pol.sources, defaultRole, pol.matchMode); err != nil {
				return fmt.Errorf("invalid user policy: %w", err)
			}
			req := rbac.CanRequest{Subject: args[0], Action: args[1], Resource: args[2], Strict: strict}
			if len(args) > 3 {
				req.Object = args[3]
			}
			decision, err := enf.Can(req)
			if err != nil {
				return fmt.Errorf("error in RBAC request: %w", err)
			}
			if decision.Allowed {
				if !quiet {
					_, _ = fmt.Fprintln(f.out(), "Yes")
				}
				return nil
			}
			if !quiet {
				_, _ = fmt.Fprintln(f.out(), "No")
			}
			return exitWith(1, nil)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&policyFile, "policy-file", "", "path to the policy file to use")
	fl.StringVar(&defaultRole, "default-role", "", "name of the default role to use")
	fl.BoolVar(&useBuiltin, "use-builtin-policy", true, "whether to also use builtin-policy")
	fl.BoolVar(&strict, "strict", true, "whether to perform strict check on action and resource names")
	fl.BoolVarP(&quiet, "quiet", "q", false, "quiet mode - do not print results to stdout")
	return cmd
}

func newRBACValidateCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var policyFile string
	cmd := &cobra.Command{
		Use:   "validate [--policy-file POLICYFILE] [--namespace NAMESPACE]",
		Short: "Validate RBAC policy",
		Long: `
Validates an RBAC policy for being syntactically correct. The policy must be
a local file or a K8s ConfigMap in the provided namespace, and in either CSV or K8s ConfigMap format.
`,
		Example: `
# Check whether a given policy file is valid using a local policy.csv file.
patrol admin settings rbac validate --policy-file policy.csv

# Policy file can also be K8s config map with data keys like patrol-rbac-cm,
# i.e. 'policy.csv' and (optionally) 'policy.default'
patrol admin settings rbac validate --policy-file patrol-rbac-cm.yaml

# If --policy-file is not given, and instead --namespace is given the ConfigMap 'patrol-rbac-cm'
# from K8s is used.
patrol admin settings rbac validate --namespace patrol

# Either --policy-file or --namespace must be given.
`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := exactlyOnePolicySource(kube, policyFile); err != nil {
				return err
			}
			pol, err := loadPolicy(c.Context(), kube, policyFile)
			if err != nil {
				return err
			}
			if pol.empty() {
				return errors.New("Policy is empty or could not be loaded.") //nolint:staticcheck // upstream message
			}
			problems, err := rbac.Validate(pol.sources, rbac.ValidateOptions{MatchMode: pol.matchMode, Builtin: rbac.BuiltinPolicy()})
			for _, p := range problems {
				if p.Severity == rbac.SeverityWarning {
					_, _ = fmt.Fprintf(f.errOut(), "warning: %s\n", p.Error())
				}
			}
			if err != nil {
				_, _ = fmt.Fprintf(f.out(), "Policy is invalid: %v\n", err)
				return exitWith(1, nil)
			}
			_, _ = fmt.Fprint(f.out(), "Policy is valid.\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&policyFile, "policy-file", "", "path to the policy file to use")
	return cmd
}
