package stack

import (
	"fmt"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	fakescm "github.com/juliosaraiva/patrol-cd/pkg/scm/fake"
)

// AppSetParams configure the ApplicationSet controller of a Stack (the
// applicationsetcontroller.* parameters).
type AppSetParams struct {
	// Policy is applicationsetcontroller.policy ("" = sync).
	Policy v1alpha1.ApplicationsSyncPolicy
	// DisablePolicyOverride sets applicationsetcontroller.enable.policy.override=false
	// (the override is enabled by default when no policy is configured).
	DisablePolicyOverride bool
	// ProgressiveSyncs is applicationsetcontroller.enable.progressive.syncs.
	ProgressiveSyncs bool
	// Namespaces is applicationsetcontroller.namespaces (ApplicationSets in any namespace).
	Namespaces []string
	// SCMProviders enables the SCM Provider and Pull Request generators with a fake SCM
	// provider (pkg/scm/fake) registered as type "fake" (Stack.FakeSCM); AllowedSCMProviders
	// is applicationsetcontroller.allowed.scm.providers.
	SCMProviders        bool
	AllowedSCMProviders []string
}

// FakeSCM is the fake SCM provider of AppSetParams.SCMProviders (SCMProvider objects of type
// "fake" and generators referencing them through providerRef reach it).
func (s *Stack) FakeSCM() *fakescm.Provider {
	if s.scm == nil {
		s.T.Fatal("FakeSCM requires AppSetParams.SCMProviders")
	}
	return s.scm
}

// appSetRunConfig maps AppSetParams onto the controller configuration.
func (s *Stack) appSetRunConfig() (appset.RunConfig, error) {
	p := s.opts.AppSetParams
	rc := appset.RunConfig{RestConfig: s.control.Config}
	rc.Namespace = s.Namespace
	rc.Policy = p.Policy
	if rc.Policy == "" {
		rc.Policy = v1alpha1.ApplicationsSyncPolicySync
	}
	rc.EnablePolicyOverride = !p.DisablePolicyOverride
	rc.EnableProgressiveSyncs = p.ProgressiveSyncs
	rc.ApplicationSetNamespaces = p.Namespaces
	rc.MaxResourcesStatusCount = appset.DefaultMaxResourcesStatusCount
	if p.SCMProviders {
		reg := scm.NewRegistry()
		if err := fakescm.Register(reg, s.scm); err != nil {
			return rc, fmt.Errorf("register the fake SCM provider: %w", err)
		}
		rc.Generators.SCM = generators.SCMConfig{Enabled: true, AllowedProviders: p.AllowedSCMProviders, Registry: reg}
	}
	return rc, nil
}
