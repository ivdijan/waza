package nativesnapshot

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCollectionCaptureRawWireAdmission(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{"duplicate outer member", `{"kind":"first","kind":"second"}`, "duplicate"},
		{"duplicate nested snapshot member", `{"snapshot":{"baseline":{},"baseline":{}}}`, "duplicate"},
		{"duplicate blob member", `{"blobs":{"same":"","same":""}}`, "duplicate"},
		{"case alias", `{"Kind":"waza.release-native-capture"}`, "exact spelling"},
		{"unknown member", `{"unexpected":true}`, "unknown field"},
		{"malformed", `{"kind":`, "invalid JSON"},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture, err := ReadCollectionCapture(t.Context(), []byte(test.raw))
			require.ErrorContains(t, err, test.want)
			require.Nil(t, capture)
		})
	}
}
