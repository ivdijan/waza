//go:build darwin || linux

package nativetask

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCollectionTapeStreamBoundaries(t *testing.T) {
	for _, delta := range []int{0, 1} {
		t.Run(string(rune('0'+delta)), func(t *testing.T) {
			_, admitted, _, _, record := collectionFixture(t)
			parent, err := os.OpenRoot(t.TempDir())
			require.NoError(t, err)
			defer func() { require.NoError(t, parent.Close()) }()
			tape, err := CreateCollectionTape(t.Context(), parent, "native")
			require.NoError(t, err)
			defer func() { require.NoError(t, tape.Close()) }()
			require.Equal(t, CollectionStreamBytes, tape.streamLimit)
			require.NoError(t, tape.Start(t.Context(), admitted))
			raw := encoded(t, record)
			raw = append(raw, bytes.Repeat([]byte{' '}, CollectionStreamBytes-1+delta-len(raw))...)
			require.Len(t, raw, CollectionStreamBytes-1+delta)
			err = tape.Complete(t.Context(), raw)
			payload, readErr := tape.root.ReadFile(payloadFile)
			require.NoError(t, readErr)
			events, readErr := tape.root.ReadFile(eventFile)
			require.NoError(t, readErr)
			if delta == 0 {
				require.NoError(t, err)
				require.Len(t, payload, CollectionStreamBytes, "newline is part of the exact public byte threshold")
				prefix, err := InspectCollectionPrefix(t.Context(), events, payload, []*Admitted{admitted})
				require.NoError(t, err)
				require.Len(t, prefix.Records, 1)
				_, err = InspectPrefix(t.Context(), events, payload, []*Admitted{admitted})
				require.ErrorContains(t, err, "1 MiB")
			} else {
				require.ErrorContains(t, err, "stream exceeds")
				require.Empty(t, payload, "no payload bytes may be appended after a capacity failure")
				require.Equal(t, 1, bytes.Count(events, []byte{'\n'}), "only the durable start remains")
				require.True(t, tape.poisoned)
				require.Error(t, tape.Start(t.Context(), admitted))
				prefix, err := InspectCollectionPrefix(t.Context(), events, payload, []*Admitted{admitted})
				require.NoError(t, err)
				require.NotNil(t, prefix.Pending)
			}
		})
	}
}

func TestCollectionTapeChecksEventCapacityBeforeAnyCompletionWrite(t *testing.T) {
	for _, phase := range []string{"start", "terminal"} {
		for _, delta := range []int{0, 1} {
			t.Run(phase+string(rune('0'+delta)), func(t *testing.T) {
				_, admitted, _, _, record := collectionFixture(t)
				parent, err := os.OpenRoot(t.TempDir())
				require.NoError(t, err)
				defer func() { require.NoError(t, parent.Close()) }()
				tape, err := CreateCollectionTape(t.Context(), parent, "native")
				require.NoError(t, err)
				defer func() { require.NoError(t, tape.Close()) }()
				binding, err := admitted.Binding()
				require.NoError(t, err)
				event := Event{Type: "start", Admission: binding}
				raw := encoded(t, record)
				if phase == "terminal" {
					require.NoError(t, tape.Start(t.Context(), admitted))
					digest, row, err := recordDigests(raw)
					require.NoError(t, err)
					event = Event{Type: "terminal", Admission: binding, State: record.Summary.Category, RecordDigest: digest, RowDigest: row}
				}
				data, err := tape.eventData(event)
				require.NoError(t, err)
				tape.eventBytes = CollectionStreamBytes - len(data) + delta
				before, err := tape.root.ReadFile(eventFile)
				require.NoError(t, err)
				if phase == "start" {
					err = tape.Start(t.Context(), admitted)
				} else {
					err = tape.Complete(t.Context(), raw)
				}
				after, readErr := tape.root.ReadFile(eventFile)
				require.NoError(t, readErr)
				payload, readErr := tape.root.ReadFile(payloadFile)
				require.NoError(t, readErr)
				if delta == 0 {
					require.NoError(t, err)
					require.Equal(t, CollectionStreamBytes, tape.eventBytes)
					require.Equal(t, append(before, data...), after)
				} else {
					require.ErrorContains(t, err, "stream exceeds")
					require.Equal(t, before, after)
					require.Empty(t, payload, "terminal capacity is checked before writing a payload")
					require.True(t, tape.poisoned)
				}
			})
		}
	}

}

func TestCollectionTapeAdmissionBudgetBeforeStart(t *testing.T) {
	for _, delta := range []int{0, 1} {
		t.Run(string(rune('0'+delta)), func(t *testing.T) {
			_, admitted, _, _, _ := collectionFixture(t)
			remaining, err := prefixAdmissionRemaining(admitted, collectionEncodedBytes, true)
			require.NoError(t, err)
			parent, err := os.OpenRoot(t.TempDir())
			require.NoError(t, err)
			defer func() { require.NoError(t, parent.Close()) }()
			tape, err := CreateCollectionTape(t.Context(), parent, "native")
			require.NoError(t, err)
			defer func() { require.NoError(t, tape.Close()) }()
			tape.admissionRemaining = collectionEncodedBytes - remaining - delta
			err = tape.Start(t.Context(), admitted)
			raw, readErr := tape.root.ReadFile(eventFile)
			require.NoError(t, readErr)
			if delta == 0 {
				require.NoError(t, err)
				require.Zero(t, tape.admissionRemaining)
				require.NotEmpty(t, raw)
			} else {
				require.ErrorContains(t, err, "admission")
				require.Empty(t, raw)
				require.True(t, tape.poisoned)
			}
		})
	}
}
