package app

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// summary writes one "label value" line (upstream printOpFmtStr layout); values are
// sanitized (they come from the server).
func summary(w io.Writer, label, value string) {
	output.Printf(w, "%-20s%s\n", label, value)
}

// toCells converts strings to table cells.
func toCells(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// Output formats of the app commands.
const (
	outWide         = "wide"
	outJSON         = "json"
	outYAML         = "yaml"
	outName         = "name"
	outTree         = "tree"
	outTreeDetailed = "tree=detailed"
	outID           = "id"
)

var (
	commitSHARegex    = regexp.MustCompile(`^[0-9A-Fa-f]{40}$`)
	truncatedSHARegex = regexp.MustCompile(`^[0-9A-Fa-f]{7,}$`)
)

// problemConditionTypes are the Application condition types reported as active problems
// (status True). Patrol's aggregate types (Reconciled, Synced, Healthy) are not shown in
// condition tables, matching upstream where conditions only describe errors and warnings.
var problemConditionTypes = map[string]bool{
	v1alpha1.ApplicationConditionDeletionError:           true,
	v1alpha1.ApplicationConditionInvalidSpecError:        true,
	v1alpha1.ApplicationConditionComparisonError:         true,
	v1alpha1.ApplicationConditionSyncError:               true,
	v1alpha1.ApplicationConditionUnknownError:            true,
	v1alpha1.ApplicationConditionSharedResourceWarning:   true,
	v1alpha1.ApplicationConditionRepeatedResourceWarning: true,
	v1alpha1.ApplicationConditionExcludedResourceWarning: true,
	v1alpha1.ApplicationConditionOrphanedResourceWarning: true,
}

// activeConditions returns the active error and warning conditions of an Application.
func activeConditions(app *v1alpha1.Application) []metav1.Condition {
	var out []metav1.Condition
	for _, c := range app.Status.Conditions {
		if problemConditionTypes[c.Type] && c.Status == metav1.ConditionTrue {
			out = append(out, c)
		}
	}
	return out
}

// appServer returns the destination server, or its name when no server is set.
func appServer(spec *v1alpha1.ApplicationSpec) string {
	if spec.Destination.Server == "" {
		return spec.Destination.Name
	}
	return spec.Destination.Server
}

// formatSyncPolicy is the SYNCPOLICY column of `app list`.
func formatSyncPolicy(app *v1alpha1.Application) string {
	if !app.Spec.IsAutomatedSyncEnabled() {
		return "Manual"
	}
	if app.Spec.SyncPolicy.Automated.GetPrune() {
		return "Auto-Prune"
	}
	return "Auto"
}

// formatConditionsSummary is the CONDITIONS column of `app list`: condition types with
// their count when repeated, sorted, or <none>.
func formatConditionsSummary(app *v1alpha1.Application) string {
	counts := map[string]int{}
	for _, c := range activeConditions(app) {
		counts[c.Type]++
	}
	items := make([]string, 0, len(counts))
	for t, n := range counts {
		if n > 1 {
			items = append(items, fmt.Sprintf("%s(%d)", t, n))
		} else {
			items = append(items, t)
		}
	}
	if len(items) == 0 {
		return "<none>"
	}
	sort.Strings(items)
	return strings.Join(items, ",")
}

// printApplicationTable prints `app list` (and `appset generate`) tables.
func printApplicationTable(w io.Writer, apps []v1alpha1.Application, format string) {
	tw := output.NewTable(w)
	headers := []string{"NAME", "CLUSTER", "NAMESPACE", "PROJECT", "STATUS", "HEALTH", "SYNCPOLICY", "CONDITIONS"}
	wide := format == outWide
	if wide {
		headers = append(headers, "REPO", "PATH", "TARGET")
	}
	tw.Row(toCells(headers)...)
	for i := range apps {
		app := &apps[i]
		vals := []string{
			app.QualifiedName(), appServer(&app.Spec), app.Spec.Destination.Namespace, app.Spec.GetProject(),
			string(app.Status.Sync.Status), string(app.Status.Health.Status), formatSyncPolicy(app), formatConditionsSummary(app),
		}
		if wide {
			src := firstSource(&app.Spec)
			vals = append(vals, src.RepoURL, src.Path, src.TargetRevision)
		}
		tw.Row(toCells(vals)...)
	}
	_ = tw.Flush()
}

// printApplicationNames prints one qualified name per line.
func printApplicationNames(w io.Writer, apps []v1alpha1.Application) {
	for i := range apps {
		_, _ = fmt.Fprintln(w, apps[i].QualifiedName())
	}
}

// syncWindowSummary is the sync window part of the summary.
type syncWindowSummary struct {
	Status   string
	Assigned []string
}

func windowSummary(r *apiclient.ApplicationSyncWindowsResponse) syncWindowSummary {
	if r == nil {
		return syncWindowSummary{Status: "Sync Allowed"}
	}
	s := syncWindowSummary{Status: r.Status}
	if s.Status == "" {
		s.Status = "Sync Allowed"
		if !r.CanSync {
			s.Status = "Sync Denied"
		}
	}
	for _, w := range r.AssignedWindows {
		s.Assigned = append(s.Assigned, w.Kind+":"+w.Schedule+":"+w.Duration)
	}
	return s
}

// printAppSummary prints the header of `app get` / `app wait` (upstream
// printAppSummaryTable).
func printAppSummary(w io.Writer, app *v1alpha1.Application, appURL string, windows *syncWindowSummary) {
	summary(w, "Name:", app.QualifiedName())
	summary(w, "Project:", app.Spec.GetProject())
	summary(w, "Server:", appServer(&app.Spec))
	summary(w, "Namespace:", app.Spec.Destination.Namespace)
	summary(w, "URL:", appURL)
	if app.Spec.HasMultipleSources() {
		_, _ = fmt.Fprintln(w, "Sources:")
	} else {
		_, _ = fmt.Fprintln(w, "Source:")
	}
	for _, src := range app.Spec.GetSources() {
		printSourceDetails(w, &src)
	}
	status := "Sync Allowed"
	var assigned []string
	if windows != nil {
		status, assigned = windows.Status, windows.Assigned
	}
	summary(w, "SyncWindow:", status)
	if len(assigned) > 0 {
		summary(w, "Assigned Windows:", strings.Join(assigned, ","))
	}
	syncPolicy := "Manual"
	if app.Spec.IsAutomatedSyncEnabled() {
		syncPolicy = "Automated"
		if app.Spec.SyncPolicy.Automated.GetPrune() {
			syncPolicy += " (Prune)"
		}
	}
	summary(w, "Sync Policy:", syncPolicy)
	target := firstSource(&app.Spec).TargetRevision
	syncStatus := string(app.Status.Sync.Status)
	switch app.Status.Sync.Status {
	case v1alpha1.SyncStatusCodeSynced:
		syncStatus += " to " + target
	case v1alpha1.SyncStatusCodeOutOfSync:
		syncStatus += " from " + target
	}
	if !commitSHARegex.MatchString(target) && !truncatedSHARegex.MatchString(target) && len(app.Status.Sync.Revision) > 7 {
		syncStatus += fmt.Sprintf(" (%s)", app.Status.Sync.Revision[:7])
	}
	summary(w, "Sync Status:", syncStatus)
	summary(w, "Health Status:", string(app.Status.Health.Status))
}

func printSourceDetails(w io.Writer, src *v1alpha1.ApplicationSource) {
	summary(w, "- Repo:", src.RepoURL)
	summary(w, "  Target:", src.TargetRevision)
	if src.Path != "" {
		summary(w, "  Path:", src.Path)
	}
	if src.Chart != "" {
		summary(w, "  Chart:", src.Chart)
	}
	if isRefSource(src) {
		summary(w, "  Ref:", src.Ref)
	}
	if src.Helm != nil && len(src.Helm.ValueFiles) > 0 {
		summary(w, "  Helm Values:", strings.Join(src.Helm.ValueFiles, ","))
	}
	if src.Kustomize != nil && src.Kustomize.NamePrefix != "" {
		summary(w, "  Name Prefix:", src.Kustomize.NamePrefix)
	}
}

// printAppConditions prints the condition table.
func printAppConditions(w io.Writer, conditions []metav1.Condition) {
	tw := output.NewTable(w)
	tw.Row("CONDITION", "MESSAGE", "LAST TRANSITION")
	for _, c := range conditions {
		tw.Row(c.Type, c.Message, c.LastTransitionTime)
	}
	_ = tw.Flush()
}

// printOperationState prints the operation summary (upstream printOperationResult).
func printOperationState(w io.Writer, st *v1alpha1.ApplicationOperationState, now time.Time) {
	if st == nil {
		return
	}
	if st.SyncResult != nil {
		summary(w, "Operation:", "Sync")
		if len(st.SyncResult.Sources) > 0 && len(st.SyncResult.Revisions) > 0 {
			summary(w, "Sync Revision:", strings.Join(st.SyncResult.Revisions, ", "))
		} else {
			summary(w, "Sync Revision:", st.SyncResult.Revision)
		}
	}
	summary(w, "Phase:", string(st.Phase))
	summary(w, "Start:", formatTime(st.StartedAt))
	summary(w, "Finished:", formatTime(st.FinishedAt))
	var d time.Duration
	if st.StartedAt != nil {
		end := now
		if st.FinishedAt != nil {
			end = st.FinishedAt.Time
		}
		d = time.Duration(end.Unix()-st.StartedAt.Unix()) * time.Second
	}
	summary(w, "Duration:", d.String())
	if st.Message != "" {
		summary(w, "Message:", st.Message)
	}
}

func formatTime(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return "<nil>"
	}
	return t.String()
}

// resourceState is one row of the resource tables (upstream resourceState).
type resourceState struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	Status    string
	Health    string
	Hook      string
	Message   string
}

func (rs *resourceState) key() string {
	return rs.Group + "/" + rs.Kind + "/" + rs.Namespace + "/" + rs.Name
}

// merge copies non-empty differing fields of n into rs and reports whether any changed.
func (rs *resourceState) merge(n *resourceState) bool {
	updated := false
	for _, f := range []struct{ cur, nw *string }{
		{&rs.Status, &n.Status}, {&rs.Health, &n.Health}, {&rs.Hook, &n.Hook}, {&rs.Message, &n.Message},
	} {
		if *f.nw != "" && *f.cur != *f.nw {
			*f.cur = *f.nw
			updated = true
		}
	}
	return updated
}

// resourceStates combines the latest operation's per-resource results with
// status.resources (upstream getResourceStates): results first (hooks keep their phase and
// result, managed resources take their current sync and health), then the remaining
// resources sorted by key. Selectors filter the rows.
func resourceStates(app *v1alpha1.Application, results []v1alpha1.ResourceResult, selectors []resourceSelector) []*resourceState {
	byKey := map[string]v1alpha1.ResourceStatus{}
	keyOf := func(g, k, ns, n string) string { return g + "/" + k + "/" + ns + "/" + n }
	for _, r := range app.Status.Resources {
		byKey[keyOf(r.Group, r.Kind, r.Namespace, r.Name)] = r
	}
	var states []*resourceState
	for _, res := range results {
		k := keyOf(res.Group, res.Kind, res.Namespace, res.Name)
		syncStatus, health := string(res.HookPhase), string(res.Status)
		if r, ok := byKey[k]; ok && res.HookType == "" {
			health = ""
			if r.Health != nil {
				health = string(r.Health.Status)
			}
			syncStatus = string(r.Status)
		}
		states = append(states, &resourceState{
			Group: res.Group, Kind: res.Kind, Namespace: res.Namespace, Name: res.Name,
			Status: syncStatus, Health: health, Hook: string(res.HookType), Message: res.Message,
		})
		delete(byKey, k)
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := byKey[k]
		health := ""
		if r.Health != nil {
			health = string(r.Health.Status)
		}
		states = append(states, &resourceState{
			Group: r.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, Status: string(r.Status), Health: health,
		})
	}
	if len(selectors) > 0 {
		filtered := states[:0]
		for _, s := range states {
			if includeResource(selectors, s.Group, s.Kind, s.Namespace, s.Name) {
				filtered = append(filtered, s)
			}
		}
		states = filtered
	}
	return states
}

// printAppResources prints the resource table of `app get` / `app wait`.
func printAppResources(w io.Writer, states []*resourceState) {
	tw := output.NewTable(w)
	tw.Row("GROUP", "KIND", "NAMESPACE", "NAME", "STATUS", "HEALTH", "HOOK", "MESSAGE")
	for _, r := range states {
		tw.Row(r.Group, r.Kind, r.Namespace, r.Name, r.Status, r.Health, r.Hook, r.Message)
	}
	_ = tw.Flush()
}

// truncateString shortens str to num runes, ending with "...".
func truncateString(str string, num int) string {
	if utf8.RuneCountInString(str) <= num {
		return str
	}
	if num > 3 {
		num -= 3
	}
	return string([]rune(str)[:num]) + "..."
}

// printHelmParams prints Helm parameters (`--show-params`).
func printHelmParams(w io.Writer, helm *v1alpha1.ApplicationSourceHelm) {
	const paramLenLimit = 80
	_, _ = fmt.Fprintln(w)
	tw := output.NewTable(w)
	if helm != nil {
		_, _ = fmt.Fprintln(w)
		tw.Row("NAME", "VALUE")
		for _, p := range helm.Parameters {
			tw.Row(p.Name, truncateString(p.Value, paramLenLimit))
		}
	}
	_ = tw.Flush()
}

// printHistoryIDs prints the history IDs, one per line.
func printHistoryIDs(w io.Writer, history v1alpha1.RevisionHistories) {
	for _, h := range history {
		_, _ = fmt.Fprintln(w, h.ID)
	}
}

// printHistoryTable prints the history grouped by source repository (upstream
// printApplicationHistoryTable).
func printHistoryTable(w io.Writer, history v1alpha1.RevisionHistories) {
	const maxRevisionChars = 7
	type row struct {
		id       int64
		date     string
		revision string
	}
	byRepo := map[string][]row{}
	var repos []string
	add := func(repo string, r row) {
		if _, ok := byRepo[repo]; !ok {
			repos = append(repos, repo)
		}
		byRepo[repo] = append(byRepo[repo], r)
	}
	for _, h := range history {
		if len(h.Sources) > 0 {
			for i, src := range h.Sources {
				rev := src.TargetRevision
				if len(h.Revisions) == len(h.Sources) && len(h.Revisions[i]) >= maxRevisionChars {
					rev = fmt.Sprintf("%s (%s)", rev, h.Revisions[i][:maxRevisionChars])
				}
				add(src.RepoURL, row{h.ID, h.DeployedAt.String(), rev})
			}
			continue
		}
		var src v1alpha1.ApplicationSource
		if h.Source != nil {
			src = *h.Source
		}
		rev := src.TargetRevision
		if len(h.Revision) >= maxRevisionChars {
			rev = fmt.Sprintf("%s (%s)", rev, h.Revision[:maxRevisionChars])
		}
		add(src.RepoURL, row{h.ID, h.DeployedAt.String(), rev})
	}
	tw := output.NewTable(w)
	for i, repo := range repos {
		tw.Row("SOURCE", repo)
		tw.Row("ID", "DATE", "REVISION")
		for _, r := range byRepo[repo] {
			tw.Row(r.id, r.date, r.revision)
		}
		if i < len(repos)-1 {
			tw.Row("")
		}
	}
	_ = tw.Flush()
}
