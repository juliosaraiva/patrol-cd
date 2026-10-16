package app

import "strings"

// parseQualifiedName splits "namespace/name" (upstream argo.ParseFromQualifiedName). A
// name without a namespace part gets defaultNamespace; a qualified name always wins over
// the -N flag.
func parseQualifiedName(qualified, defaultNamespace string) (name, namespace string) {
	if ns, n, ok := strings.Cut(qualified, "/"); ok {
		return n, ns
	}
	return qualified, defaultNamespace
}

// qualify returns "namespace/name" when namespace is set, otherwise name.
func qualify(name, namespace string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}
