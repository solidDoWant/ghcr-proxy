package chart_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

const repoRoot = "../.."

func readRepoFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(repoRoot, path))
	require.NoError(t, err)

	return string(content)
}

// renderChart renders the whole chart with its default values and returns its objects.
func renderChart(t *testing.T) []map[string]any {
	t.Helper()

	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}

	var stderr bytes.Buffer

	cmd := exec.CommandContext(t.Context(), helm, "template", "registrygate", chartDir, "--namespace", "registrygate")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, stderr.String())

	var objects []map[string]any

	for _, document := range strings.Split(string(out), "\n---\n") {
		var object map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(document), &object))

		if len(object) != 0 {
			objects = append(objects, object)
		}
	}

	return objects
}

func TestShippedArtifactsUseTheRegistrygateNames(t *testing.T) {
	t.Run("go module", func(t *testing.T) {
		assert.Contains(t, strings.SplitN(readRepoFile(t, "go.mod"), "\n", 2)[0], "module github.com/solidDoWant/registrygate")
	})

	t.Run("binaries", func(t *testing.T) {
		for _, binary := range []string{"registrygate", "docker-credential-registrygate"} {
			_, err := os.Stat(filepath.Join(repoRoot, "cmd", binary, "main.go"))
			require.NoError(t, err, binary)
			assert.Contains(t, readRepoFile(t, "Makefile"), `-o "$(BIN_DIR)/`+binary+`" ./cmd/`+binary, binary)
		}
	})

	t.Run("images", func(t *testing.T) {
		script := readRepoFile(t, "scripts/build-images.sh")
		assert.Contains(t, script, "ghcr.io/soliddowant/registrygate:${version}")
		assert.Contains(t, script, "ghcr.io/soliddowant/registrygate-credential-helper:${version}")

		var images []string

		for _, object := range renderChart(t) {
			if object["kind"] != "Deployment" {
				continue
			}

			containers, _, err := unstructured.NestedSlice(object, "spec", "template", "spec", "containers")
			require.NoError(t, err)

			for _, container := range containers {
				images = append(images, container.(map[string]any)["image"].(string))
			}
		}

		require.NotEmpty(t, images)

		for _, image := range images {
			assert.True(t, strings.HasPrefix(image, "ghcr.io/soliddowant/registrygate:"), image)
		}
	})

	t.Run("chart", func(t *testing.T) {
		var chart struct {
			Name string `json:"name"`
		}

		require.NoError(t, yaml.Unmarshal([]byte(readRepoFile(t, "deploy/charts/registrygate/Chart.yaml")), &chart))
		assert.Equal(t, "registrygate", chart.Name)
		assert.Contains(t, readRepoFile(t, "scripts/release.sh"), "oci://ghcr.io/soliddowant/charts/registrygate:$version")
	})

	t.Run("API group and kinds", func(t *testing.T) {
		kinds := map[string]bool{}

		for _, object := range renderChart(t) {
			if object["kind"] != "CustomResourceDefinition" {
				continue
			}

			spec := object["spec"].(map[string]any)
			assert.Equal(t, "registrygate.soliddowant.dev", spec["group"])
			kinds[spec["names"].(map[string]any)["kind"].(string)] = true
		}

		assert.Equal(t, map[string]bool{
			"UpstreamRegistry": true, "UpstreamCredential": true, "RegistryMirror": true, "RegistryAccessPolicy": true,
		}, kinds, "GHCRAccessPolicy is gone")
	})
}
