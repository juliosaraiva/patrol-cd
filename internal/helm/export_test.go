package helm

// Test-only access to unexported helpers.
var (
	CleanSetParameter   = cleanSetParameter
	ClosestIndexEntry   = closestIndexEntry
	DedupKV             = dedupKV
	FlattenValues       = flattenValues
	ResolveReferenceURL = resolveReferenceURL
	TemplateArgs        = templateArgs
)
