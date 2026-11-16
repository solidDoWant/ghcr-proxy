//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	docsChartRef     = "oci://ghcr.io/soliddowant/charts/ghcr-proxy"
	docsProxyNS      = "ghcr-proxy"
	docsUpstreamCA   = "upstream-ca"
	docsBlockTimeout = 10 * time.Minute
)

// docsOrder is the order the admin pages are read in. Every page with a marked
// block must be listed.
var docsOrder = []string{"install.md", "ci-pods.md"}

var docsBlock = regexp.MustCompile("(?ms)^([ \\t]*)<!-- e2e -->\\n[ \\t]*```sh\\n(.*?)\\n[ \\t]*```$")

// docsBlocks returns the shell blocks marked for the walk-through, in the order
// a reader meets them.
func docsBlocks(t *testing.T) []string {
	t.Helper()

	dir := filepath.Join(cluster.repoRoot, "docs/admin")

	pages, err := filepath.Glob(filepath.Join(dir, "*.md"))
	require.NoError(t, err)

	var blocks []string

	for _, name := range docsOrder {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)

		for _, match := range docsBlock.FindAllStringSubmatch(string(raw), -1) {
			var lines []string
			for _, line := range strings.Split(match[2], "\n") {
				lines = append(lines, strings.TrimPrefix(line, match[1]))
			}

			blocks = append(blocks, strings.Join(lines, "\n"))
		}
	}

	for _, page := range pages {
		if slicesContains(docsOrder, filepath.Base(page)) {
			continue
		}

		raw, err := os.ReadFile(page)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "<!-- e2e -->", "%s has marked blocks but is not in docsOrder", page)
	}

	return blocks
}

func slicesContains(list []string, item string) bool {
	for _, entry := range list {
		if entry == item {
			return true
		}
	}

	return false
}

// docsShims puts a helm wrapper first on PATH. The wrapper points the chart
// reference of the docs at the chart in this repository and adds the values
// overlay; every other helm command runs unchanged.
func docsShims(t *testing.T, dir, overlay string) {
	t.Helper()

	realHelm, err := exec.LookPath("helm")
	require.NoError(t, err)

	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
args=()
swapped=0
for arg in "$@"; do
  if [ "$arg" = %q ]; then
    arg=%q
    swapped=1
  fi
  args+=("$arg")
done
if [ "$swapped" = 1 ]; then
  args+=(--values %q)
fi
exec %q "${args[@]}"
`, docsChartRef, filepath.Join(cluster.repoRoot, "deploy/charts/ghcr-proxy"), overlay, realHelm)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "helm"), []byte(script), 0o755))
}

// docsReset removes what the other e2e tests installed, so the walk-through
// starts from a cluster that has only the fake GHCR.
func docsReset(ctx context.Context, t *testing.T) {
	t.Helper()

	for _, release := range [][2]string{{"ghcr-proxy", proxyNamespace}, {"ghcr-proxy", docsProxyNS}, {"cert-manager", "cert-manager"}} {
		out, err := exec.CommandContext(ctx, "helm", "uninstall", release[0], "--kube-context", cluster.kubeContext, "--namespace", release[1], "--ignore-not-found", "--wait").CombinedOutput()
		require.NoError(t, err, string(out))
	}

	for _, namespace := range []string{proxyNamespace, ciNamespace, docsProxyNS, "cert-manager"} {
		_, err := cluster.kubectl(ctx, "", "delete", "namespace", namespace, "--ignore-not-found", "--wait", "--timeout=300s")
		require.NoError(t, err)
	}
}

func TestAdminDocsWalkThroughEndsWithWorkingDockerPush(t *testing.T) {
	ctx := t.Context()

	docsReset(ctx, t)

	work := t.TempDir()
	bin := filepath.Join(work, "bin")
	require.NoError(t, os.Mkdir(bin, 0o755))

	// The three things the docs cannot hold: the PAT of the fake GHCR as a file,
	// the values that point the proxy at the fake, and the fake's CA. The chart
	// comes from this repository because no release exists yet.
	require.NoError(t, os.WriteFile(filepath.Join(work, "pat.txt"), []byte(cluster.pat), 0o600))

	tag := cluster.proxyImage[strings.LastIndex(cluster.proxyImage, ":")+1:]
	overlay := filepath.Join(work, "overlay.yaml")
	require.NoError(t, os.WriteFile(overlay, fmt.Appendf(nil, "upstream:\n  url: https://%s\n  ca:\n    existingSecret: %s\nimage:\n  tag: %q\n  pullPolicy: Never\n", fakeRegistry, docsUpstreamCA, tag), 0o600))
	docsShims(t, bin, overlay)

	kubeconfig, err := exec.CommandContext(ctx, "kubectl", "config", "view", "--minify", "--flatten", "--context", cluster.kubeContext).Output()
	require.NoError(t, err)

	kubeconfigPath := filepath.Join(work, "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfigPath, kubeconfig, 0o600))

	env := append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBECONFIG="+kubeconfigPath,
	)
	for name, value := range cluster.podImages() {
		env = append(env, name+"="+value)
	}

	blocks := docsBlocks(t)
	require.NotEmpty(t, blocks)

	for number, block := range blocks {
		if strings.Contains(block, "helm install ghcr-proxy") {
			require.NoError(t, cluster.secret(ctx, docsProxyNS, docsUpstreamCA, "Opaque", map[string][]byte{"ca.crt": cluster.fakeCA}))
		}

		blockCtx, cancel := context.WithTimeout(ctx, docsBlockTimeout)
		cmd := exec.CommandContext(blockCtx, "bash", "-eo", "pipefail", "-c", block)
		cmd.Dir = work
		cmd.Env = env
		out, err := cmd.CombinedOutput()

		cancel()
		require.NoError(t, err, "docs block %d failed:\n%s\n\n%s", number+1, block, out)
	}

	code, err := cluster.kubectl(ctx, "", "-n", ciNamespace, "get", "pod", "build", "-o",
		`jsonpath={.status.containerStatuses[?(@.name=="docker")].state.terminated.exitCode}`)
	require.NoError(t, err)
	assert.Equal(t, "0", strings.TrimSpace(code))
}
