//go:build e2e

package e2e

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushToHarborUsesARobotThePodNeverSees(t *testing.T) {
	tenant := cluster.tenants[0]
	project := harborPushProject(tenant.namespace)
	ref := fmt.Sprintf("%s/%s/app-%s:1", cluster.harborHostName, project, cluster.runID)

	_, err := cluster.docker(t.Context(), tenant.namespace, buildScript(ref)+" && docker push "+ref)
	require.NoError(t, err, "the push must succeed with the robot held by the proxy")

	require.Eventually(t, func() bool {
		return slices.Contains(harborRepositories(t, project), "app-"+cluster.runID)
	}, 2*time.Minute, 2*time.Second, "Harbor must hold the pushed image in %s", project)

	// The robot is in the admin namespace and nowhere in the namespace of the pod.
	secrets, err := cluster.kubectl(t.Context(), "", "-n", tenant.namespace, "get", "secrets", "-o", "go-template={{range .items}}{{range $key, $value := .data}}{{$value}}{{\"\\n\"}}{{end}}{{end}}")
	require.NoError(t, err)

	for line := range strings.SplitSeq(secrets, "\n") {
		if line == "" {
			continue
		}

		decoded, err := base64.StdEncoding.DecodeString(line)
		require.NoError(t, err)
		assert.NotContains(t, string(decoded), tenant.pushRobot.username)
		assert.NotContains(t, string(decoded), tenant.pushRobot.password)
	}

	admin, err := cluster.kubectl(t.Context(), "", "-n", proxyNamespace, "get", "secret", "harbor-"+tenant.namespace, "-o", "name")
	require.NoError(t, err)
	assert.NotEmpty(t, admin)
}

func TestPushToHarborOutsideTheCredentialCeilingIsRefused(t *testing.T) {
	tenant := cluster.tenants[0]
	other := harborPushProject(cluster.tenants[1].namespace)
	ref := fmt.Sprintf("%s/%s/app-%s:2", cluster.harborHostName, other, cluster.runID)

	_, err := cluster.docker(t.Context(), tenant.namespace, buildScript(ref))
	require.NoError(t, err)

	_, err = cluster.docker(t.Context(), tenant.namespace, "docker push "+ref)
	require.Error(t, err)
	assert.NotContains(t, harborRepositories(t, other), "app-"+cluster.runID)
}

func TestDockerHubNamesPullAndBuildThroughTheMirror(t *testing.T) {
	hub := cluster.upstreams[2]
	hub.seedImage(t, "library/alpine:latest")

	tenant := cluster.tenants[0]

	_, err := cluster.docker(t.Context(), tenant.namespace, "docker pull alpine")
	require.NoError(t, err, "a bare name resolves to registry-1.docker.io")

	// Remove the local copy so the build has to fetch the base image.
	_, err = cluster.docker(t.Context(), tenant.namespace, "docker rmi alpine")
	require.NoError(t, err)

	_, err = cluster.docker(t.Context(), tenant.namespace, `d=$(mktemp -d); cd "$d"; echo x > x; printf 'FROM docker.io/library/alpine\nCOPY x /x\n' > Dockerfile; docker build --pull --no-cache -t local/hub:1 .`)
	require.NoError(t, err)

	project := harborProject(tenant.namespace, hub)

	require.Eventually(t, func() bool {
		return slices.Contains(harborRepositories(t, project), "library/alpine")
	}, 2*time.Minute, 2*time.Second, "Harbor must list library/alpine in %s", project)
}
