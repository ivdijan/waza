package controlledcomparison_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/microsoft/waza/internal/controlledcomparison"
	"github.com/microsoft/waza/internal/evidence"
	"github.com/microsoft/waza/internal/nativetask"
	"github.com/microsoft/waza/internal/releasepolicy"
	"github.com/microsoft/waza/internal/webapi"
	"github.com/stretchr/testify/require"
)

func nativeHTTPCollection(t *testing.T, directory string) webapi.ReleaseCollection {
	t.Helper()
	mux := http.NewServeMux()
	webapi.RegisterReleaseRoutes(mux, filepath.Dir(directory))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/release-collections", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	var items []webapi.ReleaseCollection
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &items))
	require.Len(t, items, 1)
	return items[0]
}

func TestNativeCollectionPublicPayloads(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native durable creation requires Linux or macOS")
	}
	for _, name := range []string{"carriage-return", "large-output", "large-request", "stream-overflow"} {
		t.Run(name, func(t *testing.T) {
			source := nativeFixture(t, "fixed-model")
			design := nativeDesign()
			output := "hello\r=1+1"
			prompt := "say hello"
			if name == "large-output" {
				output = "hello" + strings.Repeat("x", 400<<10)
			}
			if name == "large-request" {
				prompt = "say hello\n" + strings.Repeat("xxxxxxxx\n", (1<<20)/8)
				task := "id: task-0\nname: native\ngolden: true\ninputs:\n  prompt_file: prompt.txt\n"
				require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(source.EvalPath), "task.yaml"), []byte(task), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(source.EvalPath), "prompt.txt"), []byte(prompt), 0o600))
			}
			if name == "stream-overflow" {
				output = "hello" + strings.Repeat("x", 768<<10)
				raw, err := os.ReadFile(source.EvalPath)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(source.EvalPath, bytes.Replace(raw, []byte("trials_per_task: 1"), []byte("trials_per_task: 8"), 1), 0o600))
				design.Clusters[0].Tasks[0].TrialOrdinals = []int{1, 2, 3, 4, 5, 6, 7, 8}
			}
			policy, err := controlledcomparison.PlanNative(t.Context(), source, source, design, nativeRequirements(), nil)
			require.NoError(t, err)
			policy = fixedNativePolicy(t, policy)
			directory := filepath.Join(t.TempDir(), "native")
			calls := 0
			err = controlledcomparison.CollectNativeWithOptions(t.Context(), policy, source, source, directory,
				controlledcomparison.NativeCollectorOptions{NewCopilotClient: nativeFactory(t, &calls, func(_ int, client *nativeTestClient) {
					client.session.output = output
					client.session.expectedPrompt = prompt
				})})
			if name == "stream-overflow" {
				require.ErrorContains(t, err, "stream exceeds")
				require.Less(t, calls, 16, "capacity failure must prevent all subsequent runtime starts")
				for _, file := range []string{"native-task-events.ndjson", "native-task-rows.ndjson"} {
					info, err := os.Stat(filepath.Join(directory, "native", file))
					require.NoError(t, err)
					require.LessOrEqual(t, info.Size(), int64(nativetask.CollectionStreamBytes))
				}
				decision, err := controlledcomparison.ReadSelectedDecision(t.Context(), directory, policy)
				require.ErrorContains(t, err, "incomplete prefix")
				require.False(t, decision.Accepted)
				require.NotEqual(t, "invalid", decision.Compatibility.State)
				require.Len(t, decision.Native.Attempts, calls)
				last := decision.Native.Attempts[len(decision.Native.Attempts)-1]
				require.Equal(t, "incomplete", last.Status)
				require.Equal(t, "unavailable", last.OutputAvailability)
				require.Nil(t, decision.Usage[last.Key.Arm][0].Value, "the unretained final session usage must remain unknown")
				stop, err := os.ReadFile(filepath.Join(directory, "native-stop.json"))
				require.NoError(t, err)
				require.Contains(t, string(stop), "stream exceeds")
				item := nativeHTTPCollection(t, directory)
				require.False(t, item.Decision.Accepted)
				require.Len(t, item.Decision.Native.Attempts, calls)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 2, calls)
			decision, err := controlledcomparison.ReadSelectedDecision(t.Context(), directory, policy)
			require.NoError(t, err)
			require.False(t, decision.Accepted)
			require.Equal(t, "observed", decision.Operations.State)
			require.Equal(t, "inconclusive", decision.Statistics.State)
			item := nativeHTTPCollection(t, directory)
			require.Empty(t, item.Error)
			require.Len(t, item.Decision.Native.Attempts, 2)
			for _, attempt := range item.Decision.Native.Attempts {
				require.Equal(t, output, *attempt.Output)
				require.Equal(t, "passed", attempt.Status)
			}
			if name == "carriage-return" {
				assertNativeAPI(t, filepath.Dir(directory), "carriage-return")
			}
			if name == "large-output" {
				rows, err := os.ReadFile(filepath.Join(directory, "native", "native-task-rows.ndjson"))
				require.NoError(t, err)
				require.Greater(t, len(rows), 1<<20, "exercise the actual previously rejected public stream size")
				_, err = nativetask.InspectPrefix(t.Context(), nil, rows, nil)
				require.ErrorContains(t, err, "1 MiB", "private prefix stream cap remains unchanged")
			}
		})
	}
}

func TestNativeCollectionDiscoveryOrder(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native durable creation requires Linux or macOS")
	}
	source := nativeFixture(t, "fixed-model")
	base := filepath.Dir(source.EvalPath)
	require.NoError(t, os.WriteFile(filepath.Join(base, "task.yaml"), []byte("id: z-task\nname: first\ninputs:\n  prompt: say hello\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(base, "next.yaml"), []byte("id: a-task\nname: second\ninputs:\n  prompt: say hello\n"), 0o600))
	raw, err := os.ReadFile(source.EvalPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(source.EvalPath, bytes.Replace(raw, []byte("[task.yaml]"), []byte("[task.yaml, next.yaml]"), 1), 0o600))
	design := nativeDesign()
	design.Clusters[0].Tasks = []releasepolicy.PlannedTask{{ID: "z-task", Weight: "0.5", TrialOrdinals: []int{1}}, {ID: "a-task", Weight: "0.5", TrialOrdinals: []int{1}}}
	policy, err := controlledcomparison.PlanNative(t.Context(), source, source, design, nativeRequirements(), nil)
	require.NoError(t, err)
	_, core, profile, err := controlledcomparison.DecodeNativePolicy(policy)
	require.NoError(t, err)
	require.Equal(t, "z-task", core.Arms[releasepolicy.Baseline].Plan.Tasks[0].ID)
	require.Equal(t, "a-task", profile.Arms[releasepolicy.Baseline].Requests[0].TaskID)
	directory := filepath.Join(t.TempDir(), "native")
	calls := 0
	require.NoError(t, controlledcomparison.CollectNativeWithOptions(t.Context(), policy, source, source, directory,
		controlledcomparison.NativeCollectorOptions{NewCopilotClient: nativeFactory(t, &calls)}))
	require.Equal(t, 4, calls)
	decision, err := controlledcomparison.ReadSelectedDecision(t.Context(), directory, policy)
	require.NoError(t, err)
	require.Len(t, decision.Native.Attempts, 4)
	item := nativeHTTPCollection(t, directory)
	require.Empty(t, item.Error)
	require.Len(t, item.Decision.Native.Attempts, 4)
	require.NoError(t, os.WriteFile(filepath.Join(base, "next.yaml"), []byte("id: a-task\nname: changed\ninputs:\n  prompt: drift\n"), 0o600))
	err = controlledcomparison.CollectNativeWithOptions(t.Context(), policy, source, source, filepath.Join(t.TempDir(), "drift"),
		controlledcomparison.NativeCollectorOptions{NewCopilotClient: nativeFactory(t, &calls)})
	require.Error(t, err)
	require.Equal(t, 4, calls, "drift rejects before another runtime starts")
}

func TestNativeTerminalPolicyAndLateStop(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native durable creation requires Linux or macOS")
	}
	source := nativeFixture(t, "fixed-model")
	policy, err := controlledcomparison.PlanNative(t.Context(), source, source, nativeDesign(), nativeRequirements(), nil)
	require.NoError(t, err)
	directory := filepath.Join(t.TempDir(), "native")
	calls := 0
	require.NoError(t, controlledcomparison.CollectNativeWithOptions(t.Context(), policy, source, source, directory,
		controlledcomparison.NativeCollectorOptions{NewCopilotClient: nativeFactory(t, &calls)}))
	clean, err := controlledcomparison.ReadSelectedDecision(t.Context(), directory, policy)
	require.NoError(t, err)
	require.Equal(t, "observed", clean.Operations.State)
	t.Run("late-stop", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(directory, "native-stop.json"), []byte(`{"kind":"waza.release-native-stop","version":"1.0","reason":"retained late close failure"}`), 0o600))
		stopped, err := controlledcomparison.ReadSelectedDecision(t.Context(), directory, policy)
		require.Error(t, err)
		require.Equal(t, "failed", stopped.Operations.State)
		require.Equal(t, "complete", stopped.Completeness.State)
		require.False(t, stopped.Accepted)
		require.Len(t, stopped.Native.Attempts, 2)
		item := nativeHTTPCollection(t, directory)
		require.NotEmpty(t, item.Error)
		require.Equal(t, "failed", item.Decision.Operations.State)
		require.Len(t, item.Decision.Native.Attempts, 2)
	})
	require.NoError(t, os.Remove(filepath.Join(directory, "native-stop.json")))

	journalPath := filepath.Join(directory, "journal.ndjson")
	journal, err := os.ReadFile(journalPath)
	require.NoError(t, err)
	lines := bytes.SplitAfter(journal, []byte{'\n'})
	prefix := []byte{}
	for _, line := range lines {
		prefix = append(prefix, line...)
		if bytes.Contains(line, []byte(`"type":"attempt_start"`)) {
			break
		}
	}
	require.Contains(t, string(prefix), `"type":"attempt_start"`)
	require.NotContains(t, string(prefix), `"type":"attempt_terminal"`)
	require.NoError(t, os.WriteFile(journalPath, prefix, 0o600))
	require.NoError(t, os.Remove(filepath.Join(directory, "journal.json")))
	rowPath := filepath.Join(directory, "native", "native-task-rows.ndjson")
	rows, err := os.ReadFile(rowPath)
	require.NoError(t, err)
	first := bytes.Split(rows, []byte{'\n'})[0]
	eventPath := filepath.Join(directory, "native", "native-task-events.ndjson")
	allEvents, err := os.ReadFile(eventPath)
	require.NoError(t, err)
	events := bytes.Split(allEvents, []byte{'\n'})
	for _, field := range []string{"valid", "model", "engine", "reasoning"} {
		t.Run(field, func(t *testing.T) {
			var record nativetask.Record
			require.NoError(t, json.Unmarshal(first, &record))
			switch field {
			case "model":
				record.Summary.Runtime.RequestedModel = "substituted-model"
			case "engine":
				record.Summary.Runtime.RequestedEngine = "substituted-engine"
			case "reasoning":
				record.Summary.Runtime.RequestedReasoning = "substituted-reasoning"
			}
			raw, err := json.Marshal(record)
			require.NoError(t, err)
			var terminal nativetask.Event
			require.NoError(t, json.Unmarshal(events[1], &terminal))
			terminal.RecordDigest, err = evidence.JSONDigest(json.RawMessage(raw))
			require.NoError(t, err)
			end, err := json.Marshal(terminal)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(rowPath, append(raw, '\n'), 0o600))
			stream := append(bytes.Clone(events[0]), '\n')
			stream = append(stream, end...)
			require.NoError(t, os.WriteFile(eventPath, append(stream, '\n'), 0o600))
			decision, err := controlledcomparison.ReadSelectedDecision(t.Context(), directory, policy)
			require.Error(t, err)
			require.False(t, decision.Accepted)
			if field == "valid" {
				require.Contains(t, err.Error(), "incomplete prefix")
				require.NotEqual(t, "invalid", decision.Compatibility.State)
				require.NotEqual(t, "mismatched", decision.Compatibility.State)
				require.Len(t, decision.Native.Attempts, 1)
				require.Equal(t, "hello", *decision.Native.Attempts[0].Output)
			} else {
				require.Contains(t, err.Error(), "effective settings")
				require.Equal(t, "invalid", decision.Compatibility.State)
				for _, attempt := range decision.Native.Attempts {
					require.Nil(t, attempt.Output, "invalid terminal must not be exposed as a verified observation")
				}
			}
		})
	}
}
