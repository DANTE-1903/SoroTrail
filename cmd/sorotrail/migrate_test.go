package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunMigrate_ArgumentHandling(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string // empty means no error
	}{
		{name: "no args is a usage error", args: nil, wantErr: "requires an action"},
		{name: "empty args is a usage error", args: []string{}, wantErr: "requires an action"},
		{name: "unknown action is rejected", args: []string{"sideways"}, wantErr: "unknown migrate action"},
		{name: "up help short-circuits", args: []string{"up", "--help"}},
		{name: "down help short-circuits", args: []string{"down", "--help"}},
		{name: "status help short-circuits", args: []string{"status", "--help"}},
		{name: "top-level help short-circuits", args: []string{"--help"}},
		{name: "negative steps is rejected", args: []string{"up", "--steps", "-1"}, wantErr: "must be >= 0"},
		{name: "stray positional is rejected", args: []string{"up", "extra"}, wantErr: "unexpected argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runMigrate(tt.args)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestDefaultMigrateSteps(t *testing.T) {
	assert.Equal(t, 1, defaultMigrateSteps("down"), "a bare down must be conservative")
	assert.Equal(t, 0, defaultMigrateSteps("up"), "up applies everything pending")
	assert.Equal(t, 0, defaultMigrateSteps("status"))
}

func TestDispatch_MigrateRoutesToRunMigrate(t *testing.T) {
	// "migrate" with no action is a usage error produced by runMigrate, which
	// proves dispatch routed it there rather than falling through to the
	// indexer.
	err := dispatch([]string{"migrate"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires an action")

	require.NoError(t, dispatch([]string{"migrate", "help"}))
}
