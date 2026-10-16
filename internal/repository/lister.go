package repository

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Resources of the kinds the Lister watches.
var (
	RepositoryGVR           = v1alpha1.GroupVersion.WithResource("repositories")
	RepositoryCredentialGVR = v1alpha1.GroupVersion.WithResource("repositorycredentials")
	SCMProviderGVR          = v1alpha1.GroupVersion.WithResource("scmproviders")
)

// DefaultResync is the informer resync period (upstream secrets informer: 3 minutes).
const DefaultResync = 3 * time.Minute

// DefaultSecretLabelSelector selects the Secrets referenced by Repository,
// RepositoryCredential and SCMProvider objects.
var DefaultSecretLabelSelector = fmt.Sprintf("%s in (%s,%s,%s,%s,%s)", v1alpha1.LabelKeySecretType,
	v1alpha1.LabelValueSecretTypeRepository, v1alpha1.LabelValueSecretTypeRepositoryWrite,
	v1alpha1.LabelValueSecretTypeRepoCreds, v1alpha1.LabelValueSecretTypeRepoCredsWrite,
	v1alpha1.LabelValueSecretTypeSCMCreds)

const (
	indexByURL     = "byURL"
	indexByProject = "byProject"
	indexByPurpose = "byPurpose"
)

// ListerOptions configures a Lister.
type ListerOptions struct {
	// Namespace is the control-plane namespace. Required.
	Namespace string
	// ResyncPeriod of the informers; zero means DefaultResync.
	ResyncPeriod time.Duration
	// SecretLabelSelector selects the cached Secrets; empty means DefaultSecretLabelSelector.
	SecretLabelSelector string
	// Logger receives warnings about objects that cannot be decoded. Nil discards them.
	Logger *slog.Logger
}

// Lister is an informer-backed, read-only view of the Repository, RepositoryCredential and
// SCMProvider objects of the control-plane namespace and of the Secrets they reference.
// Public lookups return deep copies; it is safe for concurrent use.
type Lister struct {
	ns       string
	logger   *slog.Logger
	selector string

	informers []cache.SharedIndexInformer
	repos     cache.Indexer
	creds     cache.Indexer
	scms      cache.Indexer
	secrets   cache.Indexer

	started atomic.Bool
	ready   atomic.Bool
}

// NewLister creates a Lister reading the Patrol kinds through dyn and Secrets through kube.
// Call Start before any lookup.
func NewLister(dyn dynamic.Interface, kube kubernetes.Interface, opts ListerOptions) (*Lister, error) {
	if dyn == nil || kube == nil {
		return nil, errors.New("repository lister: nil client")
	}
	if opts.Namespace == "" {
		return nil, errors.New("repository lister: namespace is required")
	}
	resync := opts.ResyncPeriod
	if resync <= 0 {
		resync = DefaultResync
	}
	selector := opts.SecretLabelSelector
	if selector == "" {
		selector = DefaultSecretLabelSelector
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	l := &Lister{ns: opts.Namespace, logger: logger.With("component", "repository-lister"), selector: selector}

	ri := dynamicinformer.NewFilteredDynamicInformer(dyn, RepositoryGVR, opts.Namespace, resync, cache.Indexers{
		indexByURL:     repoIndex(func(r *v1alpha1.Repository) string { return purposeKey(r.Spec.Purpose) + "\x00" + urlKey(r.Spec.URL) }),
		indexByProject: repoIndex(func(r *v1alpha1.Repository) string { return purposeKey(r.Spec.Purpose) + "\x00" + r.Spec.Project }),
	}, nil).Informer()
	ci := dynamicinformer.NewFilteredDynamicInformer(dyn, RepositoryCredentialGVR, opts.Namespace, resync, cache.Indexers{
		indexByPurpose: func(obj any) ([]string, error) {
			c, ok := obj.(*v1alpha1.RepositoryCredential)
			if !ok {
				return nil, nil
			}
			return []string{purposeKey(c.Spec.Purpose)}, nil
		},
	}, nil).Informer()
	si := dynamicinformer.NewFilteredDynamicInformer(dyn, SCMProviderGVR, opts.Namespace, resync, cache.Indexers{}, nil).Informer()
	secInf := coreinformers.NewFilteredSecretInformer(kube, opts.Namespace, resync, cache.Indexers{}, func(lo *metav1.ListOptions) {
		lo.LabelSelector = selector
	})
	transforms := []struct {
		inf cache.SharedIndexInformer
		fn  cache.TransformFunc
	}{
		{ri, convertTransform(l.logger, "Repository", func() runtime.Object { return &v1alpha1.Repository{} })},
		{ci, convertTransform(l.logger, "RepositoryCredential", func() runtime.Object { return &v1alpha1.RepositoryCredential{} })},
		{si, convertTransform(l.logger, "SCMProvider", func() runtime.Object { return &v1alpha1.SCMProvider{} })},
		{secInf, stripSecretMetadata},
	}
	for _, t := range transforms {
		if err := t.inf.SetTransform(t.fn); err != nil {
			return nil, fmt.Errorf("repository lister: set transform: %w", err)
		}
	}
	l.informers = []cache.SharedIndexInformer{ri, ci, si, secInf}
	l.repos, l.creds, l.scms, l.secrets = ri.GetIndexer(), ci.GetIndexer(), si.GetIndexer(), secInf.GetIndexer()
	return l, nil
}

func repoIndex(f func(*v1alpha1.Repository) string) cache.IndexFunc {
	return func(obj any) ([]string, error) {
		r, ok := obj.(*v1alpha1.Repository)
		if !ok {
			return nil, nil
		}
		return []string{f(r)}, nil
	}
}

// purposeKey maps the empty purpose to read (the CRD default).
func purposeKey(p v1alpha1.RepositoryPurpose) string {
	if p == "" {
		return string(v1alpha1.RepositoryPurposeRead)
	}
	return string(p)
}

// convertTransform decodes informer objects into typed objects once, at ingestion. Objects
// that do not decode are kept as unstructured and ignored by lookups.
func convertTransform(logger *slog.Logger, kind string, newObj func() runtime.Object) cache.TransformFunc {
	return func(obj any) (any, error) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return obj, nil
		}
		out := newObj()
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out); err != nil {
			logger.Warn("failed to decode object, ignoring it", "kind", kind, "name", u.GetName(), "error", err)
			return obj, nil
		}
		if m, ok := out.(metav1.Object); ok {
			m.SetManagedFields(nil)
		}
		return out, nil
	}
}

func stripSecretMetadata(obj any) (any, error) {
	if s, ok := obj.(*corev1.Secret); ok {
		s.ManagedFields = nil
		// The data is all the lister needs; dropping annotations avoids caching a
		// last-applied-configuration copy of the credentials twice.
		s.Annotations = nil
	}
	return obj, nil
}

// Start runs the informers until ctx ends and waits for their initial listing.
func (l *Lister) Start(ctx context.Context) error {
	if !l.started.CompareAndSwap(false, true) {
		return errors.New("repository lister: already started")
	}
	synced := make([]cache.InformerSynced, 0, len(l.informers))
	for _, inf := range l.informers {
		go inf.RunWithContext(ctx)
		synced = append(synced, inf.HasSynced)
	}
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return fmt.Errorf("repository lister: %w: %w", ErrNotSynced, context.Cause(ctx))
	}
	l.ready.Store(true)
	return nil
}

// HasSynced reports whether Start completed.
func (l *Lister) HasSynced() bool { return l.ready.Load() }

// Namespace returns the control-plane namespace the lister watches.
func (l *Lister) Namespace() string { return l.ns }

func (l *Lister) check() error {
	if !l.ready.Load() {
		return ErrNotSynced
	}
	return nil
}

// repositoriesForURL returns the shared (read-only) Repositories of purpose whose URL is the
// same location as repoURL, ordered by name.
func (l *Lister) repositoriesForURL(purpose v1alpha1.RepositoryPurpose, repoURL string) ([]*v1alpha1.Repository, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	objs, err := l.repos.ByIndex(indexByURL, purposeKey(purpose)+"\x00"+urlKey(repoURL))
	if err != nil {
		return nil, fmt.Errorf("repository lister: %w", err)
	}
	return sortedRepos(objs), nil
}

// allRepositories returns the shared Repositories of purpose ordered by name.
func (l *Lister) allRepositories(purpose v1alpha1.RepositoryPurpose) ([]*v1alpha1.Repository, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	var out []*v1alpha1.Repository
	want := purposeKey(purpose)
	for _, o := range l.repos.List() {
		if r, ok := o.(*v1alpha1.Repository); ok && purposeKey(r.Spec.Purpose) == want {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func sortedRepos(objs []any) []*v1alpha1.Repository {
	out := make([]*v1alpha1.Repository, 0, len(objs))
	for _, o := range objs {
		if r, ok := o.(*v1alpha1.Repository); ok {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// credentials returns the shared RepositoryCredentials of purpose ordered by name.
func (l *Lister) credentials(purpose v1alpha1.RepositoryPurpose) ([]*v1alpha1.RepositoryCredential, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	objs, err := l.creds.ByIndex(indexByPurpose, purposeKey(purpose))
	if err != nil {
		return nil, fmt.Errorf("repository lister: %w", err)
	}
	out := make([]*v1alpha1.RepositoryCredential, 0, len(objs))
	for _, o := range objs {
		if c, ok := o.(*v1alpha1.RepositoryCredential); ok {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// scmProvider returns the shared SCMProvider named name.
func (l *Lister) scmProvider(name string) (*v1alpha1.SCMProvider, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	obj, ok, err := l.scms.GetByKey(l.ns + "/" + name)
	if err != nil {
		return nil, fmt.Errorf("repository lister: %w", err)
	}
	p, isProvider := obj.(*v1alpha1.SCMProvider)
	if !ok || !isProvider {
		return nil, configError(ErrSCMProviderNotFound, "SCMProvider %q not found in namespace %q", name, l.ns)
	}
	return p, nil
}

// secretUse describes a reference to a Secret, so the Secret can be checked against it.
// Upstream keeps credentials inside the repository Secret itself; here objects reference
// Secrets by name, so the reference must not let one object borrow the credentials meant for
// another kind, purpose or project.
type secretUse struct {
	// owner names the referencing object in messages (`Repository "x"`).
	owner string
	// secretType is the required cd.patrol.example/secret-type label value.
	secretType string
	// project is spec.project of a referencing Repository ("" for unscoped objects). A
	// Secret used by a project-scoped Repository must have an ownerReference to a
	// Repository of that project.
	project string
	// ownerName and ownerUID identify the referencing Repository: a Secret it owns is bound
	// to it even before the informer has observed it.
	ownerName string
	ownerUID  types.UID
}

// repositorySecretType is the secret-type of Secrets referenced by Repositories of purpose.
func repositorySecretType(p v1alpha1.RepositoryPurpose) string {
	if purposeKey(p) == string(v1alpha1.RepositoryPurposeWrite) {
		return v1alpha1.LabelValueSecretTypeRepositoryWrite
	}
	return v1alpha1.LabelValueSecretTypeRepository
}

// credentialSecretType is the secret-type of Secrets referenced by RepositoryCredentials of
// purpose.
func credentialSecretType(p v1alpha1.RepositoryPurpose) string {
	if purposeKey(p) == string(v1alpha1.RepositoryPurposeWrite) {
		return v1alpha1.LabelValueSecretTypeRepoCredsWrite
	}
	return v1alpha1.LabelValueSecretTypeRepoCreds
}

// secretFor returns the data of the Secret named name after checking it may serve use: its
// secret-type label is use.secretType and, for a project-scoped Repository, it is bound to
// that project. The map is shared and must not be modified.
func (l *Lister) secretFor(name string, use secretUse) (map[string][]byte, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	obj, ok, err := l.secrets.GetByKey(l.ns + "/" + name)
	if err != nil {
		return nil, fmt.Errorf("repository lister: %w", err)
	}
	s, isSecret := obj.(*corev1.Secret)
	if !ok || !isSecret {
		return nil, configError(ErrSecretNotFound, "%s: secret %q not found in namespace %q (Secrets referenced by repositories must carry the label %s with one of the values repository, repository-write, repo-creds, repo-write-creds, scm-creds)",
			use.owner, name, l.ns, v1alpha1.LabelKeySecretType)
	}
	if got := s.Labels[v1alpha1.LabelKeySecretType]; got != use.secretType {
		return nil, configError(ErrInvalidConfiguration, "%s: secret %q has %s=%q but this reference requires %q",
			use.owner, name, v1alpha1.LabelKeySecretType, got, use.secretType)
	}
	if use.project != "" && !l.boundToProject(s, use) {
		return nil, configError(ErrInvalidConfiguration, "%s: secret %q is not bound to project %q (a Secret referenced by a project-scoped Repository must have an ownerReference to a Repository of that project)",
			use.owner, name, use.project)
	}
	return s.Data, nil
}

// boundToProject reports whether s has an ownerReference to the referencing Repository or
// to a Repository of use.project (matched by UID, so a recreated object does not inherit
// the binding).
func (l *Lister) boundToProject(s *corev1.Secret, use secretUse) bool {
	for _, ref := range s.OwnerReferences {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != v1alpha1.GroupVersion.Group || ref.Kind != "Repository" || ref.UID == "" {
			continue
		}
		if ref.Name == use.ownerName && ref.UID == use.ownerUID {
			return true
		}
		obj, ok, err := l.repos.GetByKey(l.ns + "/" + ref.Name)
		if err != nil || !ok {
			continue
		}
		if r, isRepo := obj.(*v1alpha1.Repository); isRepo && r.UID == ref.UID && r.Spec.Project == use.project {
			return true
		}
	}
	return false
}

// Repositories returns copies of the Repositories of purpose (empty means read), ordered by
// name.
func (l *Lister) Repositories(purpose v1alpha1.RepositoryPurpose) ([]*v1alpha1.Repository, error) {
	repos, err := l.allRepositories(purpose)
	if err != nil {
		return nil, err
	}
	out := make([]*v1alpha1.Repository, len(repos))
	for i, r := range repos {
		out[i] = r.DeepCopy()
	}
	return out, nil
}

// Repository returns a copy of the Repository named name.
func (l *Lister) Repository(name string) (*v1alpha1.Repository, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	obj, ok, err := l.repos.GetByKey(l.ns + "/" + name)
	if err != nil {
		return nil, fmt.Errorf("repository lister: %w", err)
	}
	r, isRepo := obj.(*v1alpha1.Repository)
	if !ok || !isRepo {
		return nil, fmt.Errorf("repository %q: %w", name, ErrRepositoryNotFound)
	}
	return r.DeepCopy(), nil
}

// RepositoryCredentials returns copies of the RepositoryCredentials of purpose (empty means
// read), ordered by name.
func (l *Lister) RepositoryCredentials(purpose v1alpha1.RepositoryPurpose) ([]*v1alpha1.RepositoryCredential, error) {
	creds, err := l.credentials(purpose)
	if err != nil {
		return nil, err
	}
	out := make([]*v1alpha1.RepositoryCredential, len(creds))
	for i, c := range creds {
		out[i] = c.DeepCopy()
	}
	return out, nil
}

// SCMProvider returns a copy of the SCMProvider named name (ErrSCMProviderNotFound when
// absent).
func (l *Lister) SCMProvider(name string) (*v1alpha1.SCMProvider, error) {
	p, err := l.scmProvider(name)
	if err != nil {
		return nil, err
	}
	return p.DeepCopy(), nil
}

// GetProjectRepositoryURLs returns the URLs of the read-purpose Repositories scoped to
// project, in name order. It implements internal/project.ScopedRepositoryLister (upstream
// adds them to the project's effective sourceRepos).
func (l *Lister) GetProjectRepositoryURLs(_ context.Context, project string) ([]string, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	if project == "" {
		return nil, nil
	}
	objs, err := l.repos.ByIndex(indexByProject, purposeKey(v1alpha1.RepositoryPurposeRead)+"\x00"+project)
	if err != nil {
		return nil, fmt.Errorf("repository lister: %w", err)
	}
	repos := sortedRepos(objs)
	urls := make([]string, 0, len(repos))
	for _, r := range repos {
		urls = append(urls, r.Spec.URL)
	}
	return urls, nil
}

// secretKeyValue reads one key of a Secret, reporting which object needs it when absent.
func (l *Lister) secretKeyValue(ref v1alpha1.SecretKeyReference, use secretUse) (string, error) {
	data, err := l.secretFor(ref.Name, use)
	if err != nil {
		return "", err
	}
	v, ok := data[ref.Key]
	if !ok || len(v) == 0 {
		return "", configError(ErrSecretKeyMissing, "%s: secret %q has no key %q", use.owner, ref.Name, ref.Key)
	}
	return string(v), nil
}
