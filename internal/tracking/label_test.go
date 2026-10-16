package tracking

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTruncateLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "short", in: "my-app", want: "my-app"},
		{name: "exactly 63", in: strings.Repeat("a", 63), want: strings.Repeat("a", 63)},
		{name: "short value ending in special char is not validated", in: "my-app-", want: "my-app-"},
		{
			name: "long name",
			in:   "my-app-with-an-extremely-long-name-that-is-over-sixty-three-characters",
			want: "my-app-with-an-extremely-long-name-that-is-over-sixty-three-cha",
		},
		{
			name: "trailing hyphen stripped",
			in:   "the-very-suspicious-name-with-precisely-sixty-three-characters-with-hyphen",
			want: "the-very-suspicious-name-with-precisely-sixty-three-characters",
		},
		{name: "several trailing specials stripped", in: strings.Repeat("a", 60) + "-._" + "bbbb", want: strings.Repeat("a", 60)},
		{name: "only the first char is alphanumeric", in: "a" + strings.Repeat("-", 70), want: "a"},
		{name: "all special characters", in: strings.Repeat("-", 64), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := TruncateLabel(tc.in)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrLabelTruncation)
				assert.EqualError(t, err, "unable to truncate label to not end with a special character")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, len(got), LabelMaxLength)
		})
	}
}
