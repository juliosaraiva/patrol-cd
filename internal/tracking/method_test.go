package tracking

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveMethod(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in             Method
		want           Method
		usesAnnotation bool
		valid          bool
	}{
		{in: "", want: MethodAnnotation, usesAnnotation: true, valid: true},
		{in: MethodAnnotation, want: MethodAnnotation, usesAnnotation: true, valid: true},
		{in: MethodLabel, want: MethodLabel, usesAnnotation: false, valid: true},
		{in: MethodAnnotationAndLabel, want: MethodAnnotationAndLabel, usesAnnotation: true, valid: true},
		{in: "Label", want: MethodAnnotation, usesAnnotation: true, valid: false},
		{in: "annotations", want: MethodAnnotation, usesAnnotation: true, valid: false},
	}
	for _, tc := range tests {
		t.Run(string(tc.in), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, EffectiveMethod(tc.in))
			assert.Equal(t, tc.usesAnnotation, UsesAnnotation(tc.in))
			if tc.valid {
				assert.NoError(t, ValidateMethod(tc.in))
			} else {
				assert.EqualError(t, ValidateMethod(tc.in), `invalid resource tracking method "`+string(tc.in)+
					`": must be one of "annotation", "label" or "annotation+label"`)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "zero value", cfg: Config{}},
		{name: "recommended key", cfg: Config{InstanceLabelKey: "cd.patrol.example/instance", ControlPlaneNamespace: "patrol-cd"}},
		{name: "invalid key", cfg: Config{InstanceLabelKey: "bad key/x/y"}, wantErr: `invalid instance label key "bad key/x/y"`},
		{name: "invalid namespace", cfg: Config{ControlPlaneNamespace: "Patrol_CD"}, wantErr: `invalid control-plane namespace "Patrol_CD"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	tr := New(Config{})
	assert.Equal(t, keyInstance, tr.InstanceLabelKey())
	assert.Empty(t, tr.ControlPlaneNamespace())
	assert.Equal(t, []string{keyInstance}, tr.TrackingLabelKeys())
	assert.Equal(t, []string{keyTracking, keyInstallation}, tr.TrackingAnnotationKeys())

	custom := New(Config{InstanceLabelKey: "cd.patrol.example/instance", ControlPlaneNamespace: "cp"})
	assert.Equal(t, "cd.patrol.example/instance", custom.InstanceLabelKey())
	assert.Equal(t, "cp", custom.ControlPlaneNamespace())
}
