package responder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/microsoft/waza/internal/execution"
	"github.com/microsoft/waza/internal/models"
	"github.com/stretchr/testify/require"
)

type loopbackTransport struct {
	origin string
	client *http.Transport
}

func (t *loopbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme+"://"+req.URL.Host != t.origin {
		return nil, fmt.Errorf("responder probe denied non-loopback inference")
	}
	return t.client.RoundTrip(req)
}

type terminalControlExecutor struct {
	Executor
	nonterminal bool
	callbacks   atomic.Int32
}

func (e *terminalControlExecutor) Execute(ctx context.Context, req *execution.ExecutionRequest) (*execution.ExecutionResponse, error) {
	allowed := []string{"custom:" + toolRespond, "custom:" + toolStop, "custom:" + toolAbstain}
	req.ToolPolicy = execution.NewToolPolicy(&allowed)
	for i := range req.Tools {
		if e.nonterminal {
			req.Tools[i].IsTerminal = false
		}
		handler := req.Tools[i].Handler
		req.Tools[i].Handler = func(inv copilot.ToolInvocation) (copilot.ToolResult, error) {
			e.callbacks.Add(1)
			return handler(inv)
		}
	}
	return e.Executor.Execute(ctx, req)
}

// This opt-in probe uses the real SDK and native CLI, but only synthetic
// responses from a loopback provider. It never calls a paid model.
func TestResponderTerminalRuntime(t *testing.T) {
	if os.Getenv("WAZA_RESPONDER_RUNTIME_TEST") != "1" {
		t.Skip("set WAZA_RESPONDER_RUNTIME_TEST=1 and COPILOT_CLI_PATH for the loopback runtime probe")
	}
	cli := os.Getenv("COPILOT_CLI_PATH")
	require.True(t, filepath.IsAbs(cli), "probe requires an explicit absolute native CLI path")
	info, err := os.Stat(cli)
	require.NoError(t, err)
	require.False(t, info.IsDir())
	t.Chdir(t.TempDir())
	for _, name := range []string{
		"COPILOT_BASE_URL", "COPILOT_PROVIDER_BASE_URL", "COPILOT_PROVIDER", "COPILOT_PROVIDER_TYPE",
		"COPILOT_WIRE_API", "COPILOT_PROVIDER_WIRE_API", "COPILOT_API_KEY", "COPILOT_PROVIDER_API_KEY",
		"COPILOT_BEARER_TOKEN", "COPILOT_PROVIDER_BEARER_TOKEN",
	} {
		t.Setenv(name, "")
	}

	for _, tc := range []struct {
		name string
		args string
		want Decision
	}{
		{toolRespond, `{"answer":"synthetic answer"}`, Decision{Kind: DecisionReply, Answer: "synthetic answer"}},
		{toolStop, `{}`, Decision{Kind: DecisionStop}},
		{toolAbstain, `{"reason":"synthetic missing fact"}`, Decision{Kind: DecisionAbstain, Reason: "synthetic missing fact"}},
	} {
		for _, mode := range []string{"terminal", "nonterminal", "conflict"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				conflictTool := toolStop
				conflictArgs := `{}`
				if tc.name == toolStop {
					conflictTool, conflictArgs = toolRespond, `{"answer":"conflicting answer"}`
				}
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, err := io.Copy(io.Discard, io.LimitReader(r.Body, 4<<20))
					if err != nil {
						t.Errorf("reading synthetic request: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					count := requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					message := map[string]any{"role": "assistant", "content": "unnecessary continuation"}
					finish := "stop"
					if count == 1 {
						message["content"] = nil
						calls := []any{map[string]any{
							"id": "decision", "type": "function",
							"function": map[string]any{"name": tc.name, "arguments": tc.args},
						}}
						if mode == "conflict" {
							calls = append(calls, map[string]any{
								"id": "conflicting-decision", "type": "function",
								"function": map[string]any{"name": conflictTool, "arguments": conflictArgs},
							})
						}
						message["tool_calls"] = calls
						finish = "tool_calls"
					}
					err = json.NewEncoder(w).Encode(map[string]any{
						"id": "synthetic", "object": "chat.completion", "model": "gpt-4.1",
						"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
						"usage":   map[string]int{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15},
					})
					if err != nil {
						t.Errorf("writing synthetic response: %v", err)
					}
				}))
				defer server.Close()
				t.Setenv("COPILOT_BASE_URL", server.URL+"/v1")
				t.Setenv("COPILOT_API_KEY", "synthetic-responder-key")
				t.Setenv("COPILOT_PROVIDER_TYPE", "openai")
				t.Setenv("COPILOT_WIRE_API", "completions")
				home := t.TempDir()
				transport := &loopbackTransport{origin: server.URL, client: &http.Transport{Proxy: nil}}
				defer transport.client.CloseIdleConnections()
				engine := execution.NewCopilotEngineBuilder("gpt-4.1", &execution.CopilotEngineBuilderOptions{
					DisableCommandMocks: true,
					NewCopilotClient: func(options *copilot.ClientOptions) execution.CopilotClient {
						env := []string{"HOME=" + home, "USERPROFILE=" + home, "COPILOT_HOME=" + home}
						for _, key := range []string{"SystemRoot", "WINDIR", "TEMP", "TMP", "PATH"} {
							if value, ok := os.LookupEnv(key); ok {
								env = append(env, key+"="+value)
							}
						}
						no := false
						options.Connection = copilot.StdioConnection{Path: cli, Env: env}
						options.WorkingDirectory, options.BaseDirectory = home, home
						options.Mode, options.UseLoggedInUser = copilot.ModeEmpty, &no
						options.RequestHandler = &copilot.CopilotRequestHandler{
							Transport: transport,
							OpenWebSocket: func(*copilot.CopilotRequestContext) (copilot.CopilotWebSocketHandler, error) {
								return nil, fmt.Errorf("responder probe denies WebSockets")
							},
						}
						return execution.NewOwnedCopilotClient(options)
					},
				}).Build()
				ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
				defer cancel()
				require.NoError(t, engine.Initialize(ctx))
				defer func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					require.NoError(t, engine.Shutdown(cleanup))
				}()
				control := &terminalControlExecutor{Executor: engine, nonterminal: mode == "nonterminal"}
				classifier := New(control, models.ResponderConfig{Instructions: "Use the synthetic decision."}, "gpt-4.1")
				decision, err := classifier.Classify(ctx, "Synthetic agent message.")
				if mode == "conflict" {
					require.ErrorContains(t, err, "responder tool call invalid: responder called ")
					require.ErrorContains(t, err, "after a decision was already recorded")
					require.Equal(t, Decision{}, decision)
					require.EqualValues(t, 2, control.callbacks.Load(), "both already-issued calls must reach the real recorder")
					t.Logf("same-response conflict: model requests=%d, decision callbacks=%d", requests.Load(), control.callbacks.Load())
					return
				}
				require.NoError(t, err)
				require.Equal(t, tc.want, decision)
				require.EqualValues(t, 1, control.callbacks.Load())
				wantRequests := int32(1)
				if mode == "nonterminal" {
					wantRequests = 2
				}
				require.Equal(t, wantRequests, requests.Load(), "successful terminal decision must prevent the next model request")
				t.Logf("model requests=%d, decision callbacks=%d", requests.Load(), control.callbacks.Load())
			})
		}
	}
}
