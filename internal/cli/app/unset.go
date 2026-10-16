package app

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// unsetOptions describe what to unset in a source.
type unsetOptions struct {
	namePrefix              bool
	nameSuffix              bool
	kustomizeVersion        bool
	kustomizeNamespace      bool
	kustomizeImages         []string
	kustomizeReplicas       []string
	ignoreMissingComponents bool
	parameters              []string
	valuesFiles             []string
	valuesLiteral           bool
	ignoreMissingValueFiles bool
	pluginEnvs              []string
	passCredentials         bool
	ref                     bool
}

func (o *unsetOptions) kustomizeIsZero() bool {
	return !o.namePrefix && !o.nameSuffix && !o.kustomizeVersion && !o.kustomizeNamespace &&
		!o.ignoreMissingComponents && len(o.kustomizeImages) == 0 && len(o.kustomizeReplicas) == 0
}

func newUnsetCommand(e *env) *cobra.Command {
	o := &unsetOptions{}
	var (
		sourcePosition int
		sourceName     string
		appNamespace   string
	)
	c := &cobra.Command{
		Use:   "unset APPNAME parameters",
		Short: "Unset application parameters",
		Example: `  # Unset kustomize override kustomize image
  patrol app unset my-app --kustomize-image=alpine

  # Unset kustomize override suffix
  patrol app unset my-app --namesuffix

  # Unset kustomize override suffix for source at position 1 under spec.sources of app my-app. source-position starts at 1.
  patrol app unset my-app --source-position 1 --namesuffix

  # Unset kustomize override suffix for source named "test" under spec.sources of app my-app.
  patrol app unset my-app --source-name test --namesuffix

  # Unset parameter override
  patrol app unset my-app -p COMPONENT=PARAM`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 1 {
				return usage(c, args)
			}
			if sourceName != "" && sourcePosition != -1 {
				return fatalGeneric("Only one of source-position and source-name can be specified.")
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appNamespace)
			app, err := getApp(ctx, cl, name, ns, "")
			if err != nil {
				return err
			}
			position, err := resolveSourcePosition(app, sourcePosition, sourceName, true)
			if err != nil {
				return err
			}
			// The overrides of an Application that uses a source hydrator live in its dry
			// source; they are edited on a detached view and stored back below (upstream edits
			// a copy and loses the change).
			hydrator := app.Spec.SourceHydrator
			var source *v1alpha1.ApplicationSource
			switch {
			case hydrator != nil && position > 0:
				return fatalGeneric("an application that uses a source hydrator has no source at position %d: "+
					"its overrides belong to the dry source, omit --source-position", position)
			case hydrator != nil:
				view := drySourceView(hydrator)
				source = &view
			default:
				source = sourceByPosition(&app.Spec, position)
			}
			if source == nil {
				return usage(c, args)
			}
			updated, nothingToUnset := unsetSource(source, o)
			if nothingToUnset {
				return usage(c, args)
			}
			if !updated {
				return nil
			}
			if helmIsZero(source.Helm) {
				source.Helm = nil
			}
			if kustomizeIsZero(source.Kustomize) {
				source.Kustomize = nil
			}
			if hydrator != nil {
				storeDrySource(hydrator, source)
			}
			ok, err := e.optionalPrompt().Confirm("Are you sure you want to unset the parameters? [y/n]")
			if err != nil {
				return err
			}
			if !ok {
				_, _ = fmt.Fprintln(c.OutOrStdout(), "The command to unset the parameters has been cancelled.")
				return nil
			}
			// Upstream sends validate=false for unset (its option struct is not bound to a
			// --validate flag); removing overrides must not depend on repository access.
			validate := false
			return updateSpec(ctx, cl, app, &validate)
		},
	}
	fs := c.Flags()
	fs.StringVarP(&appNamespace, "app-namespace", "N", "", "Unset application parameters in namespace")
	fs.StringArrayVarP(&o.parameters, "parameter", "p", []string{}, "Unset a parameter override (e.g. -p guestbook=image)")
	fs.StringArrayVar(&o.valuesFiles, "values", []string{}, "Unset one or more Helm values files")
	fs.BoolVar(&o.valuesLiteral, "values-literal", false, "Unset literal Helm values block")
	fs.BoolVar(&o.ignoreMissingValueFiles, "ignore-missing-value-files", false, "Unset the helm ignore-missing-value-files option (revert to false)")
	fs.BoolVar(&o.nameSuffix, "namesuffix", false, "Kustomize namesuffix")
	fs.BoolVar(&o.namePrefix, "nameprefix", false, "Kustomize nameprefix")
	fs.BoolVar(&o.kustomizeVersion, "kustomize-version", false, "Kustomize version")
	fs.BoolVar(&o.kustomizeNamespace, "kustomize-namespace", false, "Kustomize namespace")
	fs.StringArrayVar(&o.kustomizeImages, "kustomize-image", []string{}, "Kustomize images name (e.g. --kustomize-image node --kustomize-image mysql)")
	fs.StringArrayVar(&o.kustomizeReplicas, "kustomize-replica", []string{}, "Kustomize replicas name (e.g. --kustomize-replica my-deployment --kustomize-replica my-statefulset)")
	fs.BoolVar(&o.ignoreMissingComponents, "ignore-missing-components", false, "Unset the kustomize ignore-missing-components option (revert to false)")
	fs.StringArrayVar(&o.pluginEnvs, "plugin-env", []string{}, "Unset plugin env variables (e.g --plugin-env name)")
	fs.BoolVar(&o.passCredentials, "pass-credentials", false, "Unset passCredentials")
	fs.BoolVar(&o.ref, "ref", false, "Unset ref on the source")
	fs.IntVar(&sourcePosition, "source-position", -1, "Position of the source from the list of sources of the app. Counting starts at 1.")
	fs.StringVar(&sourceName, "source-name", "", "Name of the source from the list of sources of the app.")
	return c
}

// unsetSource removes the selected options from source (upstream unset). nothingToUnset
// is true when no option applicable to the source's tool was given.
func unsetSource(source *v1alpha1.ApplicationSource, o *unsetOptions) (updated, nothingToUnset bool) {
	needToUnsetRef := false
	if o.ref && isRefSource(source) {
		source.Ref = ""
		updated = true
		needToUnsetRef = true
	}
	if k := source.Kustomize; k != nil {
		if o.kustomizeIsZero() {
			return updated, !needToUnsetRef
		}
		if o.namePrefix && k.NamePrefix != "" {
			k.NamePrefix, updated = "", true
		}
		if o.nameSuffix && k.NameSuffix != "" {
			k.NameSuffix, updated = "", true
		}
		if o.kustomizeVersion && k.Version != "" {
			k.Version, updated = "", true
		}
		if o.kustomizeNamespace && k.Namespace != "" {
			k.Namespace, updated = "", true
		}
		if o.ignoreMissingComponents && k.IgnoreMissingComponents {
			k.IgnoreMissingComponents, updated = false, true
		}
		for _, image := range o.kustomizeImages {
			kept := k.Images[:0:0]
			for _, item := range k.Images {
				if imageName(image) == imageName(string(item)) {
					updated = true
					continue
				}
				kept = append(kept, item)
			}
			k.Images = kept
		}
		for _, replica := range o.kustomizeReplicas {
			for i, item := range k.Replicas {
				if item.Name == replica {
					k.Replicas = append(k.Replicas[:i:i], k.Replicas[i+1:]...)
					updated = true
					break
				}
			}
		}
	}
	if h := source.Helm; h != nil {
		if len(o.parameters) == 0 && len(o.valuesFiles) == 0 && !o.valuesLiteral && !o.ignoreMissingValueFiles && !o.passCredentials {
			return updated, !needToUnsetRef
		}
		for _, name := range o.parameters {
			for i, p := range h.Parameters {
				if p.Name == name {
					h.Parameters = append(h.Parameters[:i:i], h.Parameters[i+1:]...)
					updated = true
					break
				}
			}
		}
		if o.valuesLiteral && !helmValuesEmpty(h) {
			_ = setHelmValues(h, "")
			updated = true
		}
		for _, vf := range o.valuesFiles {
			for i, existing := range h.ValueFiles {
				if existing == vf {
					h.ValueFiles = append(h.ValueFiles[:i:i], h.ValueFiles[i+1:]...)
					updated = true
					break
				}
			}
		}
		if o.ignoreMissingValueFiles && h.IgnoreMissingValueFiles {
			h.IgnoreMissingValueFiles, updated = false, true
		}
		if o.passCredentials && h.PassCredentials {
			h.PassCredentials, updated = false, true
		}
	}
	if p := source.Plugin; p != nil {
		if len(o.pluginEnvs) == 0 {
			return false, !needToUnsetRef
		}
		for _, env := range o.pluginEnvs {
			if err := removeEnvEntry(p, env); err == nil {
				updated = true
			}
		}
	}
	return updated, false
}
