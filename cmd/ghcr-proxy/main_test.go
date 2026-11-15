package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "version flag prints the version and succeeds",
			args:       []string{"--version"},
			wantCode:   0,
			wantStdout: "ghcr-proxy 0.0.0-dev\n",
		},
		{
			name:       "config flag is accepted",
			args:       []string{"--config", "/tmp/config.yaml", "--version"},
			wantCode:   0,
			wantStdout: "ghcr-proxy 0.0.0-dev\n",
		},
		{
			name:       "missing config file fails before serving",
			args:       []string{"--config", "/nonexistent/config.yaml"},
			wantCode:   1,
			wantStdout: "ghcr-proxy 0.0.0-dev\n",
			wantStderr: "read config",
		},
		{
			name:       "unknown flag fails with usage",
			args:       []string{"--no-such-flag"},
			wantCode:   2,
			wantStderr: "flag provided but not defined",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(t.Context(), test.args, &stdout, &stderr)

			assert.Equal(t, test.wantCode, code)
			assert.Equal(t, test.wantStdout, stdout.String())
			assert.Contains(t, stderr.String(), test.wantStderr)
		})
	}
}
