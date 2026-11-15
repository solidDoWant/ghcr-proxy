package upstream_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/ghcr-proxy/internal/upstream"
)

func TestPATFileLoading(t *testing.T) {
	tests := []struct {
		name           string
		initial        *string
		replacement    *string
		wantPAT        string
		wantGeneration uint64
		wantLoaded     bool
	}{
		{name: "trims surrounding whitespace", initial: ptr("  ghp_first\n"), wantPAT: "ghp_first", wantGeneration: 1, wantLoaded: true},
		{name: "missing file is not loaded", wantLoaded: false},
		{name: "empty file is not loaded", initial: ptr(" \n"), wantLoaded: false},
		{name: "changed file starts a new generation", initial: ptr("ghp_first"), replacement: ptr("ghp_second\n"), wantPAT: "ghp_second", wantGeneration: 2, wantLoaded: true},
		{name: "unchanged file keeps the generation", initial: ptr("ghp_first"), replacement: ptr("ghp_first\n"), wantPAT: "ghp_first", wantGeneration: 1, wantLoaded: true},
		{name: "empty reload keeps the previous PAT", initial: ptr("ghp_first"), replacement: ptr(""), wantPAT: "ghp_first", wantGeneration: 1, wantLoaded: true},
		{name: "a PAT appearing later is loaded", replacement: ptr("ghp_late"), wantPAT: "ghp_late", wantGeneration: 1, wantLoaded: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if test.initial != nil {
				require.NoError(t, os.WriteFile(path, []byte(*test.initial), 0o600))
			}

			var logs bytes.Buffer
			holder := upstream.NewPATFile(path, slog.New(slog.NewTextHandler(&logs, nil)))

			if test.replacement != nil {
				require.NoError(t, os.WriteFile(path, []byte(*test.replacement), 0o600))
				holder.Reload()
			}

			pat, generation, loaded := holder.Get()
			assert.Equal(t, test.wantLoaded, loaded)
			assert.Equal(t, test.wantPAT, pat)

			if test.wantLoaded {
				assert.Equal(t, test.wantGeneration, generation)
			}

			assert.NotContains(t, logs.String(), "ghp_")
		})
	}
}

func ptr(value string) *string { return &value }
