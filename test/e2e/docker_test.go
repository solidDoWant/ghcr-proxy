//go:build e2e

package e2e

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestPodIsTheOneInTheAdminGuide(t *testing.T) {
	pod, err := cluster.ciPod("192.0.2.1")
	require.NoError(t, err)

	assert.Equal(t, "ghcr.io", pod.Spec.HostAliases[0].Hostnames[0])
	assert.Equal(t, proxyClusterIP, pod.Spec.HostAliases[0].IP)

	var projected, helper bool

	for _, volume := range pod.Spec.Volumes {
		if volume.Projected != nil {
			projected = volume.Projected.Sources[0].ServiceAccountToken.Audience == "registrygate"
		}

		helper = helper || volume.Image != nil
	}

	assert.True(t, projected, "projected token with audience registrygate")
	assert.True(t, helper, "image volume for the credential helper")

	live, err := cluster.kubectl(t.Context(), "", "-n", ciNamespace, "get", "pod", "build", "-o", "yaml")
	require.NoError(t, err)

	var running corev1.Pod
	require.NoError(t, yaml.Unmarshal([]byte(live), &running))
	assert.Equal(t, pod.Spec.Containers[0].Image, running.Spec.Containers[0].Image)
	assert.Equal(t, pod.Spec.Containers[1].Image, running.Spec.Containers[1].Image)
}

func TestChartInstallRunsNonRootWithReadOnlyRootFilesystem(t *testing.T) {
	raw, err := cluster.kubectl(t.Context(), "", "-n", proxyNamespace, "get", "pods", "-o", "json")
	require.NoError(t, err)

	var pods corev1.PodList
	require.NoError(t, json.Unmarshal([]byte(raw), &pods))
	require.Len(t, pods.Items, 2)

	for _, pod := range pods.Items {
		assert.Equal(t, corev1.PodRunning, pod.Status.Phase, pod.Name)
		require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot, pod.Name)
		assert.True(t, *pod.Spec.SecurityContext.RunAsNonRoot, pod.Name)

		for _, container := range pod.Spec.Containers {
			require.NotNil(t, container.SecurityContext.ReadOnlyRootFilesystem, container.Name)
			assert.True(t, *container.SecurityContext.ReadOnlyRootFilesystem, container.Name)
		}
	}
}
