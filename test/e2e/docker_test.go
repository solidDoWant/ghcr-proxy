//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// buildScript writes a one-file image and builds it as tag.
func buildScript(tag string) string {
	return fmt.Sprintf(`d=$(mktemp -d); cd "$d"; echo hello > hello; printf 'FROM scratch\nCOPY hello /hello\n' > Dockerfile; docker build -t %s .`, tag)
}

func TestCIPodsAreSetUpAsTheAdminGuideSays(t *testing.T) {
	for _, tenant := range cluster.tenants {
		live, err := cluster.kubectl(t.Context(), "", "-n", tenant.namespace, "get", "pod", "build", "-o", "yaml")
		require.NoError(t, err)

		var pod corev1.Pod
		require.NoError(t, yaml.Unmarshal([]byte(live), &pod))

		var intercepted []string

		for _, alias := range pod.Spec.HostAliases {
			if alias.IP == proxyClusterIP {
				intercepted = alias.Hostnames
			}
		}

		for _, host := range cluster.allHosts() {
			assert.Contains(t, intercepted, host, tenant.namespace)
		}

		var projected, helper bool

		for _, volume := range pod.Spec.Volumes {
			if volume.Projected != nil && volume.Projected.Sources[0].ServiceAccountToken != nil {
				projected = projected || volume.Projected.Sources[0].ServiceAccountToken.Audience == "registrygate"
			}

			helper = helper || volume.Image != nil
		}

		assert.True(t, projected, "projected token with audience registrygate")
		assert.True(t, helper, "image volume for the credential helper")

		config, err := cluster.docker(t.Context(), tenant.namespace, "cat ~/.docker/config.json")
		require.NoError(t, err)

		var parsed struct {
			CredHelpers map[string]string `json:"credHelpers"`
		}

		require.NoError(t, json.Unmarshal([]byte(config), &parsed))

		for _, host := range cluster.allHosts() {
			assert.Equal(t, "registrygate", parsed.CredHelpers[host], "docker-credential-registrygate configure adds each host")
		}
	}
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

func TestPullsPublicImagesFromBothUpstreams(t *testing.T) {
	for _, fake := range cluster.upstreams[:2] {
		fake.seedImage(t, "soliddowant/public:1")

		before := len(fake.log(t))
		_, err := cluster.docker(t.Context(), "ci-a", fmt.Sprintf("docker pull %s/soliddowant/public:1", fake.host))
		require.NoError(t, err, fake.host)

		requests := requestsFor(fake.log(t)[before:], "soliddowant/public")
		assert.NotEmpty(t, requests, "the pull from %s must reach its upstream", fake.host)

		for _, entry := range requests {
			assert.NotEqual(t, "pat-token", entry.Credential, "%s %s", entry.Method, entry.Path)
			assert.NotEqual(t, "pat-basic", entry.Credential, "%s %s", entry.Method, entry.Path)
		}
	}
}

func TestEachNamespacePullsThroughItsOwnHarborProject(t *testing.T) {
	first := cluster.upstreams[0]
	first.seedImage(t, mirroredRepo("base")+":1")

	for _, tenant := range cluster.tenants {
		_, err := cluster.docker(t.Context(), tenant.namespace, "docker pull ghcr.io/"+mirroredRepo("base")+":1")
		require.NoError(t, err, tenant.namespace)
	}

	// Harbor records a repository after it has served the pull.
	for _, tenant := range cluster.tenants {
		project := harborProject(tenant.namespace, first)

		require.Eventually(t, func() bool {
			return slices.Contains(harborRepositories(t, project), mirroredRepo("base"))
		}, 2*time.Minute, 2*time.Second, "Harbor must list the image in %s", project)
	}

	// The pulls were of upstream A: the projects for upstream B stay empty.
	for _, tenant := range cluster.tenants {
		assert.NotContains(t, harborRepositories(t, harborProject(tenant.namespace, cluster.upstreams[1])), mirroredRepo("base"), tenant.namespace)
	}
}

func TestBuildWithBaseImagesFromBothUpstreamsUsesTheMirrors(t *testing.T) {
	for _, fake := range cluster.upstreams[:2] {
		fake.seedImage(t, mirroredRepo("build")+":1")
	}

	tenant := cluster.tenants[1]
	script := fmt.Sprintf(`d=$(mktemp -d); cd "$d"; echo x > x
printf 'FROM ghcr.io/%[1]s:1 AS a\nFROM quay.io/%[1]s:1\nCOPY --from=a / /a/\nCOPY x /x\n' > Dockerfile
docker build --no-cache -t local/both:1 .`, mirroredRepo("build"))

	// The Dockerfile has no mirror setting and BuildKit has no configuration.
	_, err := cluster.docker(t.Context(), tenant.namespace, script)
	require.NoError(t, err)

	for _, fake := range cluster.upstreams[:2] {
		project := harborProject(tenant.namespace, fake)

		require.Eventually(t, func() bool {
			return slices.Contains(harborRepositories(t, project), mirroredRepo("build"))
		}, 2*time.Minute, 2*time.Second, "the base image from upstream %s must come through %s", fake.key, project)
	}
}

func TestPushWithCredentialReachesTheUpstream(t *testing.T) {
	first := cluster.upstreams[0]
	namespace := cluster.tenants[0].namespace

	before := len(first.log(t))
	_, err := cluster.docker(t.Context(), namespace, buildScript("ghcr.io/soliddowant/app:1")+" && docker push ghcr.io/soliddowant/app:1")
	require.NoError(t, err)

	local, err := cluster.docker(t.Context(), namespace, `docker inspect --format '{{index .RepoDigests 0}}' ghcr.io/soliddowant/app:1`)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/soliddowant/app@"+first.digest(t, "soliddowant/app:1").String(), strings.TrimSpace(local))

	var withCredential bool

	for _, entry := range requestsFor(first.log(t)[before:], "soliddowant/app") {
		withCredential = withCredential || (entry.Credential == "pat-token" && entry.Method != "GET" && entry.Method != "HEAD")
	}

	assert.True(t, withCredential, "the upstream must see the credential's identity on the push")
}

func TestPushOutsidePolicyIsRefused(t *testing.T) {
	first := cluster.upstreams[0]

	// ci-b has no credential rule.
	namespace := cluster.tenants[1].namespace
	_, err := cluster.docker(t.Context(), namespace, buildScript("ghcr.io/soliddowant/app:2"))
	require.NoError(t, err)

	before := len(first.log(t))
	_, err = cluster.docker(t.Context(), namespace, "docker push ghcr.io/soliddowant/app:2")
	require.Error(t, err)
	assert.Empty(t, requestsFor(first.log(t)[before:], "soliddowant/app"), "a refused push must not reach the upstream")
}

func TestPushesSucceedAfterAReplicaIsKilled(t *testing.T) {
	ctx := t.Context()
	namespace := cluster.tenants[0].namespace

	_, err := cluster.docker(ctx, namespace, buildScript("local/restart:1"))
	require.NoError(t, err)

	push := func(tag string) error {
		_, err := cluster.docker(ctx, namespace, fmt.Sprintf("docker tag local/restart:1 ghcr.io/soliddowant/app:%[1]s && docker push ghcr.io/soliddowant/app:%[1]s", tag))

		return err
	}

	require.NoError(t, push("before"))

	pods, err := cluster.kubectl(ctx, "", "-n", proxyNamespace, "get", "pods", "-o", "jsonpath={.items[*].metadata.name}")
	require.NoError(t, err)

	victim := strings.Fields(pods)[0]

	_, err = cluster.kubectl(ctx, "", "-n", proxyNamespace, "delete", "pod", victim, "--grace-period=0", "--force", "--wait=true")
	require.NoError(t, err)

	for index := range 3 {
		require.NoError(t, push(fmt.Sprintf("after-%d", index)), "push %d after the replica was killed", index)
	}

	_ = cluster.upstreams[0].digest(t, "soliddowant/app:after-2")
}
