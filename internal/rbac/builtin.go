package rbac

import _ "embed"

//go:embed builtin-policy.csv
var builtinPolicyCSV string

// BuiltinPolicy returns the built-in policy CSV that defines role:readonly and role:admin
// and binds the local "admin" account to role:admin. It is evaluated together with the
// user policy (policy.csv and overlays).
func BuiltinPolicy() string { return builtinPolicyCSV }
