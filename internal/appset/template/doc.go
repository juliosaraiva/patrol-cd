// Package template is the ApplicationSet templating engine: it substitutes generator parameters
// into Application templates (fasttemplate or Go text/template with a Sprig allow-list), applies
// templatePatch, interpolates the second child of a matrix generator with the parameters of the
// first, and merges generator-level templates into the ApplicationSet template.
//
// Behavior follows Argo CD v3.5.5 (applicationset/utils, applicationset/controllers/template);
// the implementation is independent. Rendering walks the JSON form of the object instead of
// reflecting over Go types: every string value and every object key is rendered independently,
// non-string scalars are kept, and null values inside objects are dropped (as upstream does when
// it walks raw JSON fields).
//
// Inputs (templates, parameters) are untrusted: every rendered string is capped at
// MaxRenderedBytes, text/template option panics are converted to errors, and the Go template
// function set omits env, expandenv and getHostByName.
package template
