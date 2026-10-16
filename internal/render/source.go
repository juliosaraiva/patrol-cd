package render

import "github.com/juliosaraiva/patrol-cd/api/v1alpha1"

// namespaceOrDefault returns helm.namespace, else kustomize.namespace, else def
// (upstream ApplicationSource.GetNamespaceOrDefault).
func namespaceOrDefault(s *v1alpha1.ApplicationSource, def string) string {
	if s.Helm != nil && s.Helm.Namespace != "" {
		return s.Helm.Namespace
	}
	if s.Kustomize != nil && s.Kustomize.Namespace != "" {
		return s.Kustomize.Namespace
	}
	return def
}

// kubeVersionOrDefault returns helm.kubeVersion, else kustomize.kubeVersion, else the
// cluster version def (upstream GetKubeVersionOrDefault).
func kubeVersionOrDefault(s *v1alpha1.ApplicationSource, def string) string {
	if s.Helm != nil && s.Helm.KubeVersion != "" {
		return s.Helm.KubeVersion
	}
	if s.Kustomize != nil && s.Kustomize.KubeVersion != "" {
		return s.Kustomize.KubeVersion
	}
	return def
}

// apiVersionsOrDefault returns helm.apiVersions, else kustomize.apiVersions, else the
// cluster list def; a non-empty source list replaces the cluster list.
func apiVersionsOrDefault(s *v1alpha1.ApplicationSource, def []string) []string {
	if s.Helm != nil && len(s.Helm.APIVersions) > 0 {
		return s.Helm.APIVersions
	}
	if s.Kustomize != nil && len(s.Kustomize.APIVersions) > 0 {
		return s.Kustomize.APIVersions
	}
	return def
}
