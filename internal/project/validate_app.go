package project

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Issue is a problem found in an Application spec. Type is the Argo CD condition type
// (InvalidSpecError, UnknownError) the controller reports with status True; Reason is a
// v1alpha1 Reason* constant; Message carries the upstream wording.
type Issue struct {
	Type    string
	Reason  string
	Message string
}

// Condition converts the issue to an active (status True) metav1.Condition.
func (i Issue) Condition(observedGeneration int64, now metav1.Time) metav1.Condition {
	return metav1.Condition{
		Type:               i.Type,
		Status:             metav1.ConditionTrue,
		Reason:             i.Reason,
		Message:            i.Message,
		ObservedGeneration: observedGeneration,
		LastTransitionTime: now,
	}
}

func invalidSpec(reason, format string, args ...any) Issue {
	return Issue{Type: v1alpha1.ApplicationConditionInvalidSpecError, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// ProjectErrorIssue converts a ResolveAppProject error to the controller condition (upstream
// projectErrorToCondition): a missing project is an InvalidSpecError
// "Application referencing project <p> which does not exist"; a namespace not permitted and
// any other error are UnknownError with the error text.
func ProjectErrorIssue(err error, app *v1alpha1.Application) Issue {
	if IsNotFound(err) {
		return invalidSpec(v1alpha1.ReasonProjectNotFound, "Application referencing project %s which does not exist", app.Spec.GetProject())
	}
	var np *NotPermittedError
	if errors.As(err, &np) {
		return Issue{Type: v1alpha1.ApplicationConditionUnknownError, Reason: v1alpha1.ReasonProjectNotPermitted, Message: err.Error()}
	}
	return Issue{Type: v1alpha1.ApplicationConditionUnknownError, Reason: v1alpha1.ReasonUnknownError, Message: err.Error()}
}

// ValidateOptions selects the checks of ValidateApplication beyond the upstream ones.
type ValidateOptions struct {
	// ControlPlaneNamespace is the namespace Applications may live in regardless of
	// sourceNamespaces.
	ControlPlaneNamespace string
	// SkipRefSources disables the static multi-source reference checks.
	SkipRefSources bool
	// SkipSyncOptions disables the sync option checks.
	SkipSyncOptions bool
}

// ValidateApplication returns the spec problems of app against its effective project proj
// (nil when the project does not exist). It reproduces what the controller reports on every
// refresh (upstream refreshAppConditions: project existence, namespace permission, then
// ValidatePermissions) and adds checks that need no repository access:
//
//   - multi-source references: ref names and duplicates, and Helm "$ref/..." value files
//     and file parameters pointing to defined, non-chart ref sources at one revision per
//     repository (upstream reports these from manifest generation);
//   - sync options: Key=Value syntax, known keys and values (Patrol addition, ADR-0013).
//
// An empty result means the spec is valid. lookup resolves the destination cluster.
func ValidateApplication(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, lookup ClusterLookup, opts ValidateOptions) []Issue {
	if app == nil {
		return []Issue{invalidSpec(v1alpha1.ReasonInvalidSpec, "application is nil")}
	}
	if proj == nil {
		return []Issue{ProjectErrorIssue(fmt.Errorf("project %q: %w", app.Spec.GetProject(), ErrProjectNotFound), app)}
	}
	if err := CheckAppNamespace(proj, app, opts.ControlPlaneNamespace); err != nil {
		return []Issue{ProjectErrorIssue(err, app)}
	}
	issues, err := ValidatePermissions(ctx, &app.Spec, proj, lookup)
	if err != nil {
		issues = append(issues, Issue{Type: v1alpha1.ApplicationConditionUnknownError, Reason: v1alpha1.ReasonUnknownError, Message: err.Error()})
	}
	if !opts.SkipRefSources && app.Spec.HasMultipleSources() {
		if err := ValidateRefSources(app.Spec.Sources); err != nil {
			issues = append(issues, invalidSpec(v1alpha1.ReasonInvalidSpec, "%s", err.Error()))
		}
	}
	if !opts.SkipSyncOptions && app.Spec.SyncPolicy != nil {
		for _, msg := range ValidateSyncOptions(app.Spec.SyncPolicy.SyncOptions) {
			issues = append(issues, invalidSpec(v1alpha1.ReasonInvalidSpec, "%s", msg))
		}
	}
	return issues
}

// ValidatePermissions is upstream util/argo ValidatePermissions: the source fields, the
// project's source repositories, the destination resolution and the project's destinations.
// It stops at the first missing source field and at a destination resolution failure. The
// error is an internal failure (the controller reports it as UnknownError).
func ValidatePermissions(ctx context.Context, spec *v1alpha1.ApplicationSpec, proj *v1alpha1.AppProject, lookup ClusterLookup) ([]Issue, error) {
	issues := []Issue{}
	notPermitted := func(prefix, repoURL string) Issue {
		return invalidSpec(v1alpha1.ReasonRepositoryNotPermitted, "%s %s is not permitted in project '%s'", prefix, redactURL(repoURL), proj.Name)
	}
	switch {
	case spec.SourceHydrator != nil:
		if hydratorIssues := validateSourceHydrator(spec.SourceHydrator); len(hydratorIssues) > 0 {
			return hydratorIssues, nil
		}
		dry := spec.SourceHydrator.GetDrySource()
		if !IsSourcePermitted(proj, dry.RepoURL) {
			issues = append(issues, notPermitted("application repo", dry.RepoURL))
		}
		syncSource := spec.SourceHydrator.GetSyncSource()
		if syncSource.RepoURL != spec.SourceHydrator.DrySource.RepoURL && !IsSourcePermitted(proj, syncSource.RepoURL) {
			issues = append(issues, notPermitted("sync source repo", syncSource.RepoURL))
		}
	case spec.HasMultipleSources():
		for _, source := range spec.Sources {
			if fieldIssues := validateSourceFields(source, true); len(fieldIssues) > 0 {
				return append(issues, fieldIssues...), nil
			}
			if !IsSourcePermitted(proj, source.RepoURL) {
				issues = append(issues, notPermitted("application repo", source.RepoURL))
			}
		}
	default:
		source := v1alpha1.ApplicationSource{}
		if spec.Source != nil {
			source = *spec.Source
		}
		if fieldIssues := validateSourceFields(source, false); len(fieldIssues) > 0 {
			return fieldIssues, nil
		}
		if !IsSourcePermitted(proj, source.RepoURL) {
			issues = append(issues, notPermitted("application repo", source.RepoURL))
		}
	}

	cluster, err := ResolveDestination(ctx, spec.Destination, lookup)
	if err != nil {
		return append(issues, invalidSpec(v1alpha1.ReasonDestinationNotFound, "%s", err.Error())), nil
	}
	permitted, err := IsDestinationPermitted(ctx, proj, DestinationOf(cluster, spec.Destination.Namespace), lookup)
	if err != nil {
		return nil, err
	}
	if !permitted {
		server := cluster.Spec.Server
		if spec.Destination.Name != "" {
			server = cluster.Spec.Name
		}
		issues = append(issues, invalidSpec(v1alpha1.ReasonInvalidSpec, "application destination server '%s' and namespace '%s' do not match any of the allowed destinations in project '%s'", server, spec.Destination.Namespace, proj.Name))
	}
	return issues, nil
}

func validateSourceFields(source v1alpha1.ApplicationSource, multiSource bool) []Issue {
	if multiSource {
		if source.RepoURL == "" || (source.Path == "" && source.Chart == "" && source.Ref == "") {
			return []Issue{invalidSpec(v1alpha1.ReasonInvalidSpec, "spec.source.repoURL and either source.path, source.chart, or source.ref are required for source %s", formatSource(source))}
		}
	} else if source.RepoURL == "" || (source.Path == "" && source.Chart == "") {
		return []Issue{invalidSpec(v1alpha1.ReasonInvalidSpec, "spec.source.repoURL and either spec.source.path or spec.source.chart are required")}
	}
	if source.Chart != "" && source.TargetRevision == "" {
		return []Issue{invalidSpec(v1alpha1.ReasonInvalidSpec, "spec.source.targetRevision is required if the manifest source is a helm chart")}
	}
	return nil
}

func validateSourceHydrator(h *v1alpha1.SourceHydrator) []Issue {
	var issues []Issue
	if h.DrySource.RepoURL == "" {
		issues = append(issues, invalidSpec(v1alpha1.ReasonInvalidSpec, "spec.sourceHydrator.drySource.repoURL is required"))
	}
	if h.SyncSource.TargetBranch == "" {
		issues = append(issues, invalidSpec(v1alpha1.ReasonInvalidSpec, "spec.sourceHydrator.syncSource.targetBranch is required"))
	}
	if h.HydrateTo != nil && h.HydrateTo.TargetBranch == "" {
		issues = append(issues, invalidSpec(v1alpha1.ReasonInvalidSpec, "when spec.sourceHydrator.hydrateTo is set, spec.sourceHydrator.hydrateTo.targetBranch is required"))
	}
	return issues
}

// formatSource renders a source like the upstream generated String() method, which the
// multi-source validation message embeds. Nested option structs are abbreviated.
func formatSource(s v1alpha1.ApplicationSource) string {
	nested := func(name string, isNil bool) string {
		if isNil {
			return "nil"
		}
		return "&" + name + "{...}"
	}
	return "&ApplicationSource{" +
		"RepoURL:" + redactURL(s.RepoURL) + "," +
		"Path:" + s.Path + "," +
		"TargetRevision:" + s.TargetRevision + "," +
		"Helm:" + nested("ApplicationSourceHelm", s.Helm == nil) + "," +
		"Kustomize:" + nested("ApplicationSourceKustomize", s.Kustomize == nil) + "," +
		"Directory:" + nested("ApplicationSourceDirectory", s.Directory == nil) + "," +
		"Plugin:" + nested("ApplicationSourcePlugin", s.Plugin == nil) + "," +
		"Chart:" + s.Chart + "," +
		"Ref:" + s.Ref + "," +
		"Name:" + s.Name + "," +
		"TagPrefix:" + s.TagPrefix + "," +
		"}"
}

var refKeyRegexp = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateRefSources checks the references between the sources of a multi-source Application
// without repository access, with the upstream messages:
//
//   - every ref matches ^[a-zA-Z0-9_-]+$ and is unique (upstream GetRefSources);
//   - every Helm value file or file parameter path starting with "$" names a defined ref
//     ("$<ref>/..."), the referenced source has no chart, and one source does not reference
//     the same repository at two target revisions (upstream repo-server checks).
//
// Errors are returned for the first problem found.
func ValidateRefSources(sources v1alpha1.ApplicationSources) error {
	refs := make(map[string]v1alpha1.ApplicationSource)
	for _, s := range sources {
		if s.Ref == "" {
			continue
		}
		if !refKeyRegexp.MatchString(s.Ref) {
			return fmt.Errorf("sources.ref %s cannot contain any special characters except '_' and '-'", s.Ref)
		}
		key := "$" + s.Ref
		if _, dup := refs[key]; dup {
			return errors.New("invalid sources: multiple sources had the same `ref` key")
		}
		refs[key] = s
	}
	keys := make([]string, 0, len(refs))
	for k := range refs {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	for _, s := range sources {
		if s.Helm == nil {
			continue
		}
		candidates := slices.Clone(s.Helm.ValueFiles)
		for _, fp := range s.Helm.FileParameters {
			candidates = append(candidates, fp.Path)
		}
		type seen struct{ key, revision string }
		byRepo := make(map[string]seen)
		for _, file := range candidates {
			if !strings.HasPrefix(file, "$") {
				continue
			}
			refVar, _, _ := strings.Cut(file, "/")
			target, ok := refs[refVar]
			if !ok {
				if len(refs) == 0 {
					return fmt.Errorf("source referenced %q, but no source has a 'ref' field defined", refVar)
				}
				return fmt.Errorf("source referenced %q, which is not one of the available sources (%s)", refVar, strings.Join(keys, ", "))
			}
			if target.Chart != "" {
				return errors.New("source has a 'chart' field defined, but Helm charts are not yet not supported for 'ref' sources")
			}
			repo := NormalizeGitURL(target.RepoURL)
			if prev, ok := byRepo[repo]; ok {
				if prev.revision != target.TargetRevision {
					return fmt.Errorf("cannot reference multiple revisions for the same repository (%s references %q while %s references %q)", refVar, target.TargetRevision, prev.key, prev.revision)
				}
				continue
			}
			byRepo[repo] = seen{key: refVar, revision: target.TargetRevision}
		}
	}
	return nil
}

// ValidateSourceNames rejects duplicate non-empty source names of a multi-source Application
// (upstream API check on create/update; the controller accepts duplicates).
func ValidateSourceNames(app *v1alpha1.Application) error {
	if !app.Spec.HasMultipleSources() {
		return nil
	}
	names := make(map[string]bool, len(app.Spec.Sources))
	for _, s := range app.Spec.Sources {
		if s.Name != "" && names[s.Name] {
			return fmt.Errorf("application %s has duplicate source name: %s", app.Name, s.Name)
		}
		names[s.Name] = true
	}
	return nil
}

// syncOptionSyntax is the admission rule of Application.spec.syncPolicy.syncOptions.
var syncOptionSyntax = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*=[[:graph:]]+$`)

var (
	boolValues = []string{v1alpha1.SyncOptionValueTrue, v1alpha1.SyncOptionValueFalse}
	// knownSyncOptions maps every sync option key to its accepted values.
	knownSyncOptions = map[string][]string{
		v1alpha1.SyncOptionKeyValidate:                    boolValues,
		v1alpha1.SyncOptionKeyCreateNamespace:             boolValues,
		v1alpha1.SyncOptionKeyPrune:                       {v1alpha1.SyncOptionValueTrue, v1alpha1.SyncOptionValueFalse, v1alpha1.SyncOptionValueConfirm},
		v1alpha1.SyncOptionKeyDelete:                      {v1alpha1.SyncOptionValueTrue, v1alpha1.SyncOptionValueFalse, v1alpha1.SyncOptionValueConfirm},
		v1alpha1.SyncOptionKeyPruneLast:                   boolValues,
		v1alpha1.SyncOptionKeyPrunePropagationPolicy:      {v1alpha1.SyncOptionValueForeground, v1alpha1.SyncOptionValueBackground, v1alpha1.SyncOptionValueOrphan},
		v1alpha1.SyncOptionKeyApplyOutOfSyncOnly:          boolValues,
		v1alpha1.SyncOptionKeyReplace:                     boolValues,
		v1alpha1.SyncOptionKeyForce:                       boolValues,
		v1alpha1.SyncOptionKeyServerSideApply:             boolValues,
		v1alpha1.SyncOptionKeyFailOnSharedResource:        boolValues,
		v1alpha1.SyncOptionKeyRespectIgnoreDifferences:    boolValues,
		v1alpha1.SyncOptionKeySkipDryRunOnMissingResource: boolValues,
		v1alpha1.SyncOptionKeyClientSideApplyMigration:    boolValues,
	}
)

// ValidateSyncOptions returns one message per invalid sync option: not of the form
// Key=Value, an unknown key, or a value the key does not accept. Keys and values are
// case-sensitive because the sync engine, like upstream (SyncOptions.HasOption), matches
// options verbatim and silently ignores any other spelling; a wrong-case key or value gets a
// "did you mean" hint. Upstream silently ignores invalid options; Patrol reports them
// (ADR-0013) so typos such as "CreateNamespaces=true" or "ServerSideApply=True" surface.
func ValidateSyncOptions(options v1alpha1.SyncOptions) []string {
	var msgs []string
	for _, opt := range options {
		if !syncOptionSyntax.MatchString(opt) {
			msgs = append(msgs, fmt.Sprintf("sync option %q must have the form Key=Value", opt))
			continue
		}
		key, value, _ := v1alpha1.ParseSyncOption(opt)
		allowed, known := knownSyncOptions[key]
		if !known {
			msg := fmt.Sprintf("unknown sync option %q", key)
			if k, ok := caseInsensitiveKey(key); ok {
				msg += fmt.Sprintf(" (keys are case-sensitive: did you mean %q?)", k)
			}
			msgs = append(msgs, msg)
			continue
		}
		if slices.Contains(allowed, value) {
			continue
		}
		msg := fmt.Sprintf("invalid value %q for sync option %s: must be one of %s", value, key, strings.Join(allowed, ", "))
		if i := slices.IndexFunc(allowed, func(a string) bool { return strings.EqualFold(a, value) }); i >= 0 {
			msg += fmt.Sprintf(" (values are case-sensitive: did you mean %q?)", allowed[i])
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// caseInsensitiveKey returns the known sync option key equal to key ignoring case.
func caseInsensitiveKey(key string) (string, bool) {
	for k := range knownSyncOptions {
		if strings.EqualFold(k, key) {
			return k, true
		}
	}
	return "", false
}
