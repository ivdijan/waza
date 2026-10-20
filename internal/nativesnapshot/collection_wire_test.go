package nativesnapshot

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/waza/internal/releasepolicy"
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

func TestCollectionCaptureExecutableBounds(t *testing.T) {
	base, locations := fixture(t)
	executable, err := os.Executable()
	require.NoError(t, err)
	input, err := os.Open(executable)
	require.NoError(t, err)
	output, err := os.OpenFile(filepath.Join(base, "evaluator"), os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	_, copyErr := io.Copy(output, input)
	require.NoError(t, input.Close())
	require.NoError(t, copyErr)
	info, err := output.Stat()
	require.NoError(t, err)
	size := max(info.Size(), int64(maxExecutableBytes+1))
	require.LessOrEqual(t, size, int64(CollectionExecutableBytes))
	require.NoError(t, output.Truncate(size))
	require.NoError(t, output.Close())
	_, err = Prepare(t.Context(), locations)
	require.ErrorContains(t, err, "exceeds source limit", "private executable bound stays 128 MiB")
	captured, err := CaptureCollection(t.Context(), locations)
	require.NoError(t, err, "actual evaluator image with retained trailing padding is not a substitute executable")
	require.NoError(t, captured.Recheck(t.Context()))
	prepared, err := captured.Primitive(t.Context(), releasepolicy.Baseline, "task")
	require.NoError(t, err, "public per-attempt source descriptor must admit the same evaluator")
	require.NotNil(t, prepared)
	raw, err := captured.Marshal(t.Context())
	require.NoError(t, err)
	retained, err := ReadCollectionCapture(t.Context(), raw)
	require.NoError(t, err)
	before, err := captured.Projections(t.Context())
	require.NoError(t, err)
	after, err := retained.Projections(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	t.Logf("retained actual evaluator: %d bytes; capture wire: %d bytes", size, len(raw))

	require.NoError(t, os.Truncate(filepath.Join(base, "evaluator"), CollectionExecutableBytes+1))
	_, err = CaptureCollection(t.Context(), locations)
	require.ErrorContains(t, err, "exceeds source limit", "public executable bound stays 256 MiB")
	require.NoError(t, os.Truncate(filepath.Join(base, "evaluator"), CollectionExecutableBytes))
	_, err = CaptureCollection(t.Context(), locations)
	require.ErrorContains(t, err, "candidate native capture", "the first exact-256 MiB evaluator fits its per-file allowance")
	require.ErrorContains(t, err, "captured source byte limit", "two executable inventories plus declarations exceed the 512 MiB aggregate")
}

func TestCollectionLimitSelectionAndWireBoundary(t *testing.T) {
	executable, total := sourceByteLimits(context.Background())
	require.Equal(t, 128<<20, executable)
	require.Equal(t, 256<<20, total)
	executable, total = sourceByteLimits(collectionContext(t.Context()))
	require.Equal(t, 256<<20, executable)
	require.Equal(t, 512<<20, total)
	require.NoError(t, collectionWireSize(1<<30))
	require.ErrorContains(t, collectionWireSize(1<<30+1), "retained byte limit")
	require.Nil(t, collectionContext(nil)) //nolint:staticcheck // Explicit nil-context boundary.
	_, err := CaptureCollection(nil, nil)  //nolint:staticcheck // Explicit nil-context boundary.
	require.Error(t, err)
}

// BenchmarkReadCollectionCaptureActualExecutable measures the production reader
// against a real test executable at the admitted descriptor boundary.
func BenchmarkReadCollectionCaptureActualExecutable(b *testing.B) {
	base, locations := fixture(b)
	executable, err := os.Executable()
	require.NoError(b, err)
	input, err := os.Open(executable)
	require.NoError(b, err)
	output, err := os.OpenFile(filepath.Join(base, "evaluator"), os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(b, err)
	_, copyErr := io.Copy(output, input)
	require.NoError(b, input.Close())
	require.NoError(b, copyErr)
	info, err := output.Stat()
	require.NoError(b, err)
	require.NoError(b, output.Truncate(max(info.Size(), int64(maxExecutableBytes+1))))
	require.NoError(b, output.Close())
	captured, err := CaptureCollection(b.Context(), locations)
	require.NoError(b, err)
	raw, err := captured.Marshal(b.Context())
	require.NoError(b, err)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		retained, err := ReadCollectionCapture(b.Context(), raw)
		require.NoError(b, err)
		require.NotNil(b, retained)
	}
}

func TestCollectionCaptureBufferOwnershipAndCurrentness(t *testing.T) {
	base, locations := fixture(t)
	write(t, base, "baseline/waza.lock", "")
	captured, err := CaptureCollection(t.Context(), locations)
	require.NoError(t, err)
	emptyFound := false
	for _, entry := range captured.capture.prepared.sources[releasepolicy.Baseline] {
		if entry.Path == "baseline/waza.lock" {
			emptyFound = true
			require.Nil(t, entry.Bytes, "empty sources retain their frozen nil representation")
			require.Equal(t, releasepolicy.SourceDigest(nil), entry.Digest)
		}
	}
	require.True(t, emptyFound)
	request, err := captured.Request(t.Context(), releasepolicy.Baseline, "task")
	require.NoError(t, err)
	require.Equal(t, []byte("baseline"), request.Resources[0].Content)
	request.Resources[0].Content[0] = '!'
	detached, err := captured.Request(t.Context(), releasepolicy.Baseline, "task")
	require.NoError(t, err)
	require.Equal(t, []byte("baseline"), detached.Resources[0].Content)
	require.NoError(t, captured.Recheck(t.Context()))

	write(t, base, "baseline/context/a.txt", "modified")
	require.Error(t, captured.Recheck(t.Context()), "same-length backing-file changes must invalidate currentness")
	detached, err = captured.Request(t.Context(), releasepolicy.Baseline, "task")
	require.NoError(t, err)
	require.Equal(t, []byte("baseline"), detached.Resources[0].Content)
	recaptured, err := CaptureCollection(t.Context(), locations)
	require.NoError(t, err)
	current, err := recaptured.Request(t.Context(), releasepolicy.Baseline, "task")
	require.NoError(t, err)
	require.Equal(t, []byte("modified"), current.Resources[0].Content)
}

// BenchmarkCaptureCollectionActualExecutable measures source recapture using the
// real test executable under collection-level bounds.
func BenchmarkCaptureCollectionActualExecutable(b *testing.B) {
	base, locations := fixture(b)
	executable, err := os.Executable()
	require.NoError(b, err)
	input, err := os.Open(executable)
	require.NoError(b, err)
	output, err := os.OpenFile(filepath.Join(base, "evaluator"), os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(b, err)
	_, copyErr := io.Copy(output, input)
	require.NoError(b, input.Close())
	require.NoError(b, copyErr)
	require.NoError(b, output.Close())

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		captured, err := CaptureCollection(b.Context(), locations)
		require.NoError(b, err)
		require.NotNil(b, captured)
	}
}
