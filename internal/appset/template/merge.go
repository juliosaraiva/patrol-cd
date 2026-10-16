package template

import (
	"reflect"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// MergeTemplate returns the effective template of a generator: a copy of the generator-level
// template whose zero-valued fields are filled from the ApplicationSet template (upstream
// mergo.Merge without override). Non-zero generator fields win; maps are unioned with the
// generator's non-empty entries winning; slices are taken from the ApplicationSet template only
// when empty in the generator template; pointers are merged recursively. A field cannot be
// "unset" by giving it its zero value in the generator template.
func MergeTemplate(generatorTemplate *v1alpha1.ApplicationSetTemplate, base v1alpha1.ApplicationSetTemplate) v1alpha1.ApplicationSetTemplate {
	var dst v1alpha1.ApplicationSetTemplate
	if generatorTemplate != nil {
		dst = *generatorTemplate.DeepCopy()
	}
	src := base.DeepCopy()
	mergeValue(reflect.ValueOf(&dst).Elem(), reflect.ValueOf(src).Elem())
	return dst
}

func mergeValue(dst, src reflect.Value) {
	if !src.IsValid() {
		return
	}
	switch dst.Kind() {
	case reflect.Struct:
		if !hasExportedFields(dst.Type()) {
			// Opaque structs (time.Time, ...) are only set when the destination is empty.
			if dst.CanSet() && dst.IsZero() && !src.IsZero() {
				dst.Set(src)
			}
			return
		}
		for i := 0; i < dst.NumField(); i++ {
			if !dst.Type().Field(i).IsExported() {
				continue
			}
			mergeValue(dst.Field(i), src.Field(i))
		}
	case reflect.Map:
		if src.IsNil() || src.Len() == 0 {
			return
		}
		if dst.IsNil() {
			if dst.CanSet() {
				dst.Set(reflect.MakeMapWithSize(dst.Type(), src.Len()))
			} else {
				return
			}
		}
		iter := src.MapRange()
		for iter.Next() {
			cur := dst.MapIndex(iter.Key())
			if cur.IsValid() && !isEmpty(cur) {
				continue
			}
			dst.SetMapIndex(iter.Key(), iter.Value())
		}
	case reflect.Slice:
		if dst.CanSet() && dst.Len() == 0 && src.Len() > 0 {
			dst.Set(src)
		}
	case reflect.Pointer:
		if src.IsNil() {
			return
		}
		if dst.IsNil() {
			if dst.CanSet() {
				dst.Set(src)
			}
			return
		}
		mergeValue(dst.Elem(), src.Elem())
	case reflect.Interface:
		if src.IsNil() {
			return
		}
		if dst.IsNil() && dst.CanSet() {
			dst.Set(src)
		}
	default:
		if dst.CanSet() && isEmpty(dst) && !isEmpty(src) {
			dst.Set(src)
		}
	}
}

func hasExportedFields(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			return true
		}
	}
	return false
}

// isEmpty mirrors mergo's notion of an empty value.
func isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return true
		}
		return isEmpty(v.Elem())
	case reflect.Func:
		return v.IsNil()
	case reflect.Invalid:
		return true
	default:
		return v.IsZero()
	}
}
