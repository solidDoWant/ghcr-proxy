// Package chart tests the rendered Helm chart.
package chart_test

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const chartDir = "../../deploy/charts/registrygate"

var rbacTemplates = []string{
	"templates/clusterrole.yaml",
	"templates/rbac.yaml",
	"templates/lease-role.yaml",
	"templates/secrets-role.yaml",
}

// renderRBAC renders the chart's RBAC objects and keeps the fields that decide access, so the golden file doesn't
// change with the chart version.
func renderRBAC(t *testing.T, extraArgs ...string) string {
	t.Helper()

	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}

	args := []string{"template", "registrygate", chartDir, "--namespace", "registrygate"}
	for _, template := range rbacTemplates {
		args = append(args, "--show-only", template)
	}

	args = append(args, extraArgs...)

	var stderr bytes.Buffer

	cmd := exec.CommandContext(t.Context(), helm, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, stderr.String())

	var rendered []string

	for _, document := range strings.Split(string(out), "\n---\n") {
		var object map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(document), &object))

		if len(object) == 0 {
			continue
		}

		metadata, _ := object["metadata"].(map[string]any)
		kept := map[string]any{
			"apiVersion": object["apiVersion"],
			"kind":       object["kind"],
			"metadata":   map[string]any{"name": metadata["name"]},
		}

		if namespace, ok := metadata["namespace"]; ok {
			kept["metadata"].(map[string]any)["namespace"] = namespace
		}

		for _, field := range []string{"rules", "roleRef", "subjects"} {
			if value, ok := object[field]; ok {
				kept[field] = value
			}
		}

		data, err := yaml.Marshal(kept)
		require.NoError(t, err)

		rendered = append(rendered, string(data))
	}

	return strings.Join(rendered, "---\n")
}

func TestDefaultRBACMatchesTheGoldenFile(t *testing.T) {
	assertGolden(t, "rbac.golden.yaml", renderRBAC(t))
}

func TestEachAdminNamespaceGetsASecretsRole(t *testing.T) {
	assertGolden(t, "rbac-admin-namespaces.golden.yaml",
		renderRBAC(t, "--set", "adminNamespaces={registrygate,tenants}"))
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), got)
}
