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
	nonterminal  bool
	unrestricted bool
	failFirst    bool
	callbacks    atomic.Int32
	sessionIDs   []string
}

func (e *terminalControlExecutor) Execute(ctx context.Context, req *execution.ExecutionRequest) (*execution.ExecutionResponse, error) {
	e.sessionIDs = append(e.sessionIDs, req.SessionID)
	if !e.unrestricted {
		allowed := []string{"custom:" + toolRespond, "custom:" + toolStop, "custom:" + toolAbstain}
		req.ToolPolicy = execution.NewToolPolicy(&allowed)
	}
	for i := range req.Tools {
		if e.nonterminal {
			req.Tools[i].IsTerminal = false
		}
		handler := req.Tools[i].Handler
		req.Tools[i].Handler = func(inv copilot.ToolInvocation) (copilot.ToolResult, error) {
			if e.callbacks.Add(1) == 1 && e.failFirst {
				return copilot.ToolResult{}, fmt.Errorf("synthetic transient callback failure")
			}
			return handler(inv)
		}
	}
	return e.Executor.Execute(ctx, req)
}

type stopHookControlClient struct {
	execution.CopilotClient
	onStop copilot.AgentStopHandler
}

func (c *stopHookControlClient) addHook(hooks **copilot.SessionHooks) {
	if *hooks == nil {
		*hooks = &copilot.SessionHooks{}
	}
	(*hooks).OnAgentStop = c.onStop
}

func (c *stopHookControlClient) CreateSession(ctx context.Context, config *copilot.SessionConfig) (execution.CopilotSession, error) {
	c.addHook(&config.Hooks)
	return c.CopilotClient.CreateSession(ctx, config)
}

func (c *stopHookControlClient) ResumeSessionWithOptions(ctx context.Context, id string, config *copilot.ResumeSessionConfig) (execution.CopilotSession, error) {
	c.addHook(&config.Hooks)
	return c.CopilotClient.ResumeSessionWithOptions(ctx, id, config)
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
		modes := []string{
			"terminal", "nonterminal", "terminal_duplicate", "nonterminal_duplicate", "conflict",
			"failed_callback", "default_runtime", "stop_hook_allow", "stop_hook_block", "stop_hook_duplicate",
		}
		if tc.name != toolStop {
			modes = append(modes, "invalid_then_valid")
		}
		for _, mode := range modes {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				nonterminal := mode == "nonterminal" || mode == "nonterminal_duplicate"
				duplicateNextRound := mode == "terminal_duplicate" || mode == "nonterminal_duplicate" || mode == "stop_hook_duplicate"
				failedCallback := mode == "failed_callback"
				invalidArgs := mode == "invalid_then_valid"
				defaultRuntime := mode == "default_runtime"
				hookEnabled := mode == "stop_hook_allow" || mode == "stop_hook_block" || mode == "stop_hook_duplicate"
				hookBlocks := mode == "stop_hook_block" || mode == "stop_hook_duplicate"
				conflictTool := toolStop
				conflictArgs := `{}`
				if tc.name == toolStop {
					conflictTool, conflictArgs = toolRespond, `{"answer":"conflicting answer"}`
				}
				var requests atomic.Int32
				var stops atomic.Int32
				var extraTools atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
					if err != nil {
						t.Errorf("reading synthetic request: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if defaultRuntime {
						var payload struct {
							Tools []struct {
								Function struct{ Name string } `json:"function"`
							} `json:"tools"`
						}
						if err := json.Unmarshal(body, &payload); err != nil {
							t.Errorf("decoding synthetic request: %v", err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						for _, tool := range payload.Tools {
							if name := tool.Function.Name; name != "" && name != toolRespond && name != toolStop && name != toolAbstain {
								extraTools.Store(true)
							}
						}
					}
					count := requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					message := map[string]any{"role": "assistant", "content": "unnecessary continuation"}
					finish := "stop"
					if count == 1 {
						message["content"] = nil
						args := tc.args
						if invalidArgs {
							args = `{"answer":"","reason":""}`
						}
						calls := []any{map[string]any{
							"id": "decision", "type": "function",
							"function": map[string]any{"name": tc.name, "arguments": args},
						}}
						if mode == "conflict" {
							calls = append(calls, map[string]any{
								"id": "conflicting-decision", "type": "function",
								"function": map[string]any{"name": conflictTool, "arguments": conflictArgs},
							})
						}
						message["tool_calls"] = calls
						finish = "tool_calls"
					} else if count == 2 && duplicateNextRound {
						message["content"] = nil
						message["tool_calls"] = []any{map[string]any{
							"id": "later-decision", "type": "function",
							"function": map[string]any{"name": conflictTool, "arguments": conflictArgs},
						}}
						finish = "tool_calls"
					} else if count == 2 && (failedCallback || invalidArgs) {
						message["content"] = nil
						message["tool_calls"] = []any{map[string]any{
							"id": "recovered-decision", "type": "function",
							"function": map[string]any{"name": tc.name, "arguments": tc.args},
						}}
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
						env := []string{
							"HOME=" + home, "USERPROFILE=" + home, "COPILOT_HOME=" + home,
							"APPDATA=" + home, "LOCALAPPDATA=" + home, "XDG_CONFIG_HOME=" + home,
							"COPILOT_DISABLE_KEYTAR=1", "COPILOT_AUTO_UPDATE=false", "DO_NOT_TRACK=1",
						}
						for _, key := range []string{"SystemRoot", "WINDIR", "TEMP", "TMP", "PATH"} {
							if value, ok := os.LookupEnv(key); ok {
								env = append(env, key+"="+value)
							}
						}
						no := false
						options.Connection = copilot.StdioConnection{Path: cli, Env: env}
						options.WorkingDirectory, options.BaseDirectory = home, home
						options.Mode, options.UseLoggedInUser = copilot.ModeEmpty, &no
						if defaultRuntime {
							options.Mode = copilot.ModeCopilotCli
						}
						options.RequestHandler = &copilot.CopilotRequestHandler{
							Transport: transport,
							OpenWebSocket: func(*copilot.CopilotRequestContext) (copilot.CopilotWebSocketHandler, error) {
								return nil, fmt.Errorf("responder probe denies WebSockets")
							},
						}
						client := execution.NewOwnedCopilotClient(options)
						if !hookEnabled {
							return client
						}
						return &stopHookControlClient{
							CopilotClient: client,
							onStop: func(copilot.AgentStopHookInput, copilot.HookInvocation) (*copilot.AgentStopHookOutput, error) {
								if stops.Add(1) == 1 && hookBlocks {
									return &copilot.AgentStopHookOutput{Decision: "block", Reason: "Synthetic fixture requests one continuation."}, nil
								}
								return nil, nil
							},
						}
					},
				}).Build()
				ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
				defer cancel()
				require.NoError(t, engine.Initialize(ctx))
				defer func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					require.NoError(t, engine.Shutdown(cleanup))
				}()
				control := &terminalControlExecutor{
					Executor: engine, nonterminal: nonterminal, unrestricted: defaultRuntime, failFirst: failedCallback,
				}
				classifier := New(control, models.ResponderConfig{Instructions: "Use the synthetic decision."}, "gpt-4.1")
				var sessionID string
				for turn, phase := range []string{"initial", "resumed"} {
					requests.Store(0)
					control.callbacks.Store(0)
					stops.Store(0)
					extraTools.Store(false)
					decision, err := classifier.Classify(ctx, fmt.Sprintf("Synthetic agent message %d.", turn+1))
					t.Logf("%s: model requests=%d, decision callbacks=%d, classification error=%t, stop hooks=%d",
						phase, requests.Load(), control.callbacks.Load(), err != nil, stops.Load())
					require.NotEmpty(t, classifier.sessionID)
					if turn == 0 {
						require.Empty(t, control.sessionIDs[turn])
						sessionID = classifier.sessionID
					} else {
						require.Equal(t, sessionID, control.sessionIDs[turn], "must exercise native session resumption")
						require.Equal(t, sessionID, classifier.sessionID)
					}

					if hookEnabled {
						wantStops := int32(1)
						if hookBlocks {
							wantStops = 2
						}
						require.Equal(t, wantStops, stops.Load(), "must exercise the stop hook, including native resumption")
					}
					if defaultRuntime {
						require.True(t, extraTools.Load(), "default-runtime control must expose tools beyond the decision-only fixture")
					}

					if invalidArgs {
						require.ErrorContains(t, err, tc.name+" called with empty")
						require.Equal(t, Decision{}, decision, "a later valid call must not erase the malformed-input error")
						require.EqualValues(t, 2, requests.Load(), "failed terminal tools must allow another model round")
						require.EqualValues(t, 2, control.callbacks.Load())
					} else if mode == "conflict" || mode == "nonterminal_duplicate" || mode == "stop_hook_duplicate" {
						require.ErrorContains(t, err, "responder tool call invalid: responder called ")
						require.ErrorContains(t, err, "after a decision was already recorded")
						require.Equal(t, Decision{}, decision)
						require.EqualValues(t, 2, control.callbacks.Load(), "both conflicting calls must reach the real recorder")
						if mode == "nonterminal_duplicate" || mode == "stop_hook_duplicate" {
							require.EqualValues(t, 3, requests.Load(), "ordinary tools allow the later decision and final recovery generation")
						}
					} else {
						require.NoError(t, err)
						require.Equal(t, tc.want, decision)
						wantCallbacks := int32(1)
						wantRequests := int32(1)
						if nonterminal || hookBlocks || failedCallback {
							wantRequests = 2
						}
						if failedCallback {
							wantCallbacks = 2
						}
						require.Equal(t, wantCallbacks, control.callbacks.Load())
						require.Equal(t, wantRequests, requests.Load(), "only ordinary tools, failed calls or an explicit stop-hook block should require another round")
					}
				}
				require.Len(t, control.sessionIDs, 2)
			})
		}
	}
}
