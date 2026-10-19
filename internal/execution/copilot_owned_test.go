package execution

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCopilotEngineBuilderOwnedProductionClient(t *testing.T) {
	t.Run("fresh dedicated clients without runtime startup", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "reject-copilot")
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 125\n"), 0o700))
		t.Setenv("COPILOT_CLI_PATH", path)
		first := NewCopilotEngineBuilder("fixed-model", &CopilotEngineBuilderOptions{OwnClient: true}).Build()
		second := NewCopilotEngineBuilder("fixed-model", &CopilotEngineBuilderOptions{OwnClient: true}).Build()
		require.True(t, first.ownsClient)
		require.True(t, second.ownsClient)
		require.IsType(t, &copilotClientWrapper{}, first.client)
		require.IsType(t, &copilotClientWrapper{}, second.client)
		require.NotSame(t, first.client, second.client)
		require.NoError(t, first.Shutdown(t.Context()))
		require.NoError(t, second.Shutdown(t.Context()))
	})
	t.Run("invalid explicit runtime never falls back", func(t *testing.T) {
		t.Setenv("COPILOT_CLI_PATH", filepath.Join(t.TempDir(), "missing-copilot"))
		engine := NewCopilotEngineBuilder("fixed-model", &CopilotEngineBuilderOptions{OwnClient: true}).Build()
		require.True(t, engine.ownsClient)
		require.IsType(t, &startupErrorClient{}, engine.client)
		require.ErrorContains(t, engine.Initialize(t.Context()), "COPILOT_CLI_PATH")
		require.NoError(t, engine.Shutdown(t.Context()))
	})
}
