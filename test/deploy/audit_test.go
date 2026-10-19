package deploy_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
)

// auditEvent is the subset of an audit.k8s.io/v1 Event the harness reads.
type auditEvent struct {
	Stage      string `json:"stage"`
	Verb       string `json:"verb"`
	RequestURI string `json:"requestURI"`
	User       struct {
		Username string `json:"username"`
	} `json:"user"`
	ObjectRef *struct {
		Resource    string `json:"resource"`
		Namespace   string `json:"namespace"`
		Name        string `json:"name"`
		APIGroup    string `json:"apiGroup"`
		Subresource string `json:"subresource"`
	} `json:"objectRef"`
	ResponseStatus *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	} `json:"responseStatus"`
	Annotations   map[string]string `json:"annotations"`
	RequestObject json.RawMessage   `json:"requestObject"`
}

func (e auditEvent) code() int {
	if e.ResponseStatus == nil {
		return 0
	}
	return e.ResponseStatus.Code
}

// describe renders the request for reports.
func (e auditEvent) describe() string {
	if e.ObjectRef == nil {
		return fmt.Sprintf("%s %s", e.Verb, strings.SplitN(e.RequestURI, "?", 2)[0])
	}
	r := e.ObjectRef.Resource
	if e.ObjectRef.Subresource != "" {
		r += "/" + e.ObjectRef.Subresource
	}
	if e.ObjectRef.APIGroup != "" {
		r += "." + e.ObjectRef.APIGroup
	}
	s := e.Verb + " " + r
	if e.ObjectRef.Namespace != "" {
		s += " in " + e.ObjectRef.Namespace
	}
	if e.ObjectRef.Name != "" {
		s += " name=" + e.ObjectRef.Name
	}
	return s
}

// readAudit returns the events of users matching the predicate (completed requests and
// started watches).
func readAudit(path string, user func(string) bool) ([]auditEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []auditEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e auditEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("audit log: %w", err)
		}
		if e.Stage != "ResponseComplete" && e.Stage != "ResponseStarted" && e.Stage != "Panic" {
			continue
		}
		if user(e.User.Username) {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// boundRule is one rule of a rendered Role or ClusterRole bound to a service account.
type boundRule struct {
	role      string // Role ns/name or ClusterRole name
	namespace string // "" for ClusterRoles bound by ClusterRoleBindings
	index     int
	rule      rbacv1.PolicyRule
	user      string // system:serviceaccount:<ns>:<sa>
}

func (b boundRule) String() string {
	r := b.rule
	s := fmt.Sprintf("%s rule %d: ", b.role, b.index)
	if len(r.NonResourceURLs) > 0 {
		return s + fmt.Sprintf("nonResourceURLs=%v verbs=%v", r.NonResourceURLs, r.Verbs)
	}
	s += fmt.Sprintf("apiGroups=%v resources=%v verbs=%v", r.APIGroups, r.Resources, r.Verbs)
	if len(r.ResourceNames) > 0 {
		s += fmt.Sprintf(" resourceNames=%v", r.ResourceNames)
	}
	return s
}

// boundRules returns the rules of the rendered Roles/ClusterRoles bound (by the rendered
// bindings) to service accounts of namespace ns.
func boundRules(objs []object, ns string) ([]boundRule, error) {
	roles := map[string][]rbacv1.PolicyRule{}
	for _, o := range byKind(objs, "Role", "ClusterRole") {
		var rules struct {
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		b, err := json.Marshal(o.u.Object)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &rules); err != nil {
			return nil, err
		}
		roles[o.u.GetKind()+"/"+o.u.GetNamespace()+"/"+o.u.GetName()] = rules.Rules
	}
	var out []boundRule
	for _, o := range byKind(objs, "RoleBinding", "ClusterRoleBinding") {
		var b rbacv1.RoleBinding
		raw, err := json.Marshal(o.u.Object)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		key := b.RoleRef.Kind + "/"
		if b.RoleRef.Kind == "Role" {
			key += b.Namespace
		}
		key += "/" + b.RoleRef.Name
		rules, ok := roles[key]
		if !ok {
			return nil, fmt.Errorf("%s references %s, which is not rendered", o.id(), key)
		}
		scope := b.Namespace // RoleBinding: rules apply in its namespace (also for a ClusterRole)
		roleName := strings.TrimPrefix(strings.TrimPrefix(key, "Role/"), "ClusterRole//")
		if o.u.GetKind() == "ClusterRoleBinding" {
			scope = ""
		}
		for _, s := range b.Subjects {
			if s.Kind != rbacv1.ServiceAccountKind || s.Namespace != ns {
				continue
			}
			for i, r := range rules {
				out = append(out, boundRule{role: b.RoleRef.Kind + " " + roleName, namespace: scope, index: i, rule: r, user: "system:serviceaccount:" + ns + ":" + s.Name})
			}
		}
	}
	return out, nil
}

func matchAny(list []string, v string) bool {
	return slices.Contains(list, "*") || slices.Contains(list, v)
}

// requestResource returns the request's resource ("pods/log") and API group.
func requestResource(e auditEvent) (group, resource string) {
	ref := e.ObjectRef
	resource = ref.Resource
	if ref.Subresource != "" {
		resource += "/" + ref.Subresource
	}
	return ref.APIGroup, resource
}

// covers reports which (verb, resource) cell of the rule allows the request ("" when none).
func (b boundRule) covers(e auditEvent) (verb, resource string, ok bool) {
	if e.User.Username != b.user {
		return "", "", false
	}
	verbOf := func() (string, bool) {
		switch {
		case slices.Contains(b.rule.Verbs, e.Verb):
			return e.Verb, true
		case slices.Contains(b.rule.Verbs, "*"):
			return "*", true
		}
		return "", false
	}
	v, okVerb := verbOf()
	if !okVerb {
		return "", "", false
	}
	if e.ObjectRef == nil {
		p := strings.SplitN(e.RequestURI, "?", 2)[0]
		for _, u := range b.rule.NonResourceURLs {
			if u == "*" || u == p || (strings.HasSuffix(u, "*") && strings.HasPrefix(p, strings.TrimSuffix(u, "*"))) {
				return v, u, true
			}
		}
		return "", "", false
	}
	if b.namespace != "" && e.ObjectRef.Namespace != b.namespace {
		return "", "", false
	}
	group, res := requestResource(e)
	if !matchAny(b.rule.APIGroups, group) {
		return "", "", false
	}
	if len(b.rule.ResourceNames) > 0 && !slices.Contains(b.rule.ResourceNames, e.ObjectRef.Name) {
		return "", "", false
	}
	for _, r := range b.rule.Resources {
		if r == res || r == "*" || (e.ObjectRef.Subresource != "" && r == "*/"+e.ObjectRef.Subresource) || r == e.ObjectRef.Resource+"/*" {
			return v, r, true
		}
	}
	return "", "", false
}

// impersonatedOperation reports whether an audit event is an Operation creation on behalf of
// another identity (spec.initiatedBy.username other than the requester, or automated),
// which the admission policy allows only with the "impersonate" verb.
func impersonatedOperation(e auditEvent) bool {
	if e.Verb != "create" || e.ObjectRef == nil || e.ObjectRef.Resource != "operations" || e.code() >= 300 || len(e.RequestObject) == 0 {
		return false
	}
	var op struct {
		Spec struct {
			InitiatedBy struct {
				Username  string `json:"username"`
				Automated bool   `json:"automated"`
			} `json:"initiatedBy"`
		} `json:"spec"`
	}
	if json.Unmarshal(e.RequestObject, &op) != nil {
		return false
	}
	ib := op.Spec.InitiatedBy
	return ib.Automated || (ib.Username != "" && ib.Username != e.User.Username)
}

// rbacAnalysis is the outcome of comparing audit events with the rendered RBAC.
type rbacAnalysis struct {
	forbidden   []string // denied requests not on the allow-list
	allowed403  []string // denied requests on the allow-list (with the reason)
	unexercised []string // rules that no allowed request matched (informational)
	requests    int
	users       map[string]int
}

// allowedDenial is a 403 the scenario provokes on purpose or that is documented.
type allowedDenial struct {
	user, request *regexp.Regexp
	why           string
}

func analyzeRBAC(events []auditEvent, rules []boundRule, allow []allowedDenial) rbacAnalysis {
	a := rbacAnalysis{users: map[string]int{}}
	// used[i] records the exercised (verb, resource) cells of rule i.
	used := make([]map[string]bool, len(rules))
	for i := range used {
		used[i] = map[string]bool{}
	}
	for _, e := range events {
		a.users[e.User.Username]++
		a.requests++
		if e.code() == 403 {
			desc := fmt.Sprintf("%s: %s (%s)", e.User.Username, e.describe(), strings.TrimSpace(e.ResponseStatus.Message))
			ok := false
			for _, al := range allow {
				if al.user.MatchString(e.User.Username) && al.request.MatchString(e.describe()) {
					a.allowed403 = append(a.allowed403, desc+" — allowed: "+al.why)
					ok = true
					break
				}
			}
			if !ok {
				a.forbidden = append(a.forbidden, desc)
			}
			continue
		}
		// client-go 1.36 informers use streaming lists (a watch with sendInitialEvents=true)
		// and fall back to list when the server lacks them: such a watch exercises "list" too.
		variants := []auditEvent{e}
		if e.Verb == "watch" && strings.Contains(e.RequestURI, "sendInitialEvents=true") {
			l := e
			l.Verb = "list"
			variants = append(variants, l)
		}
		for i, r := range rules {
			for _, ev := range variants {
				if v, res, ok := r.covers(ev); ok {
					used[i][v+" "+res] = true
				}
			}
			// "impersonate" on operations is checked by the admission policy's authorizer (no
			// request of its own): exercised by Operation creations on behalf of others.
			if slices.Contains(r.rule.Verbs, "impersonate") && r.user == e.User.Username && impersonatedOperation(e) &&
				matchAny(r.rule.APIGroups, "cd.patrol.example") && matchAny(r.rule.Resources, "operations") &&
				(r.namespace == "" || r.namespace == e.ObjectRef.Namespace) {
				used[i]["impersonate operations"] = true
			}
		}
	}
	for i, r := range rules {
		targets := r.rule.Resources
		if len(r.rule.NonResourceURLs) > 0 {
			targets = r.rule.NonResourceURLs
		}
		var missing []string
		for _, v := range r.rule.Verbs {
			for _, res := range targets {
				if !used[i][v+" "+res] {
					missing = append(missing, v+" "+res)
				}
			}
		}
		if len(missing) > 0 {
			a.unexercised = append(a.unexercised, fmt.Sprintf("%s: %s — never used: %s", r.user, r, strings.Join(missing, ", ")))
		}
	}
	sort.Strings(a.forbidden)
	sort.Strings(a.unexercised)
	a.forbidden = slices.Compact(a.forbidden)
	a.allowed403 = slices.Compact(a.allowed403)
	return a
}
