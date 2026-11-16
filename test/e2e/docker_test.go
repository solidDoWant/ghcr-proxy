//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// fakeEntry is one request in the fake GHCR's log.
type fakeEntry struct {
	Server     string
	Method     string
	Path       string
	Credential string
	Status     int
}

// fakeLog is the fake's request log.
func fakeLog(t *testing.T) []fakeEntry {
	t.Helper()

	raw, err := cluster.kubectl(t.Context(), "", "get", "--raw", "/api/v1/namespaces/"+fakeNamespace+"/services/fakeghcr-admin:8445/proxy/log")
	require.NoError(t, err)

	var entries []fakeEntry
	require.NoError(t, json.Unmarshal([]byte(raw), &entries))

	return entries
}

// requestsFor lists the registry requests the fake saw for a repository.
func requestsFor(entries []fakeEntry, repo string) []fakeEntry {
	var out []fakeEntry

	for _, entry := range entries {
		if entry.Server == "registry" && strings.HasPrefix(entry.Path, "/v2/"+repo+"/") {
			out = append(out, entry)
		}
	}

	return out
}

// fakeForward is a kubectl port-forward to the fake registry that is started
// again whenever a dial to it fails, because the forward dies when the pod
// restarts or its connection drops.
var fakeForward struct {
	mutex sync.Mutex
	port  string
	cmd   *exec.Cmd
}

// startFakeForward replaces the forward. The caller holds fakeForward.mutex.
func startFakeForward() error {
	if fakeForward.cmd != nil {
		_ = fakeForward.cmd.Process.Kill()
		_ = fakeForward.cmd.Wait()
	}

	cmd := exec.Command("kubectl", "--context", cluster.kubeContext, "-n", fakeNamespace, "port-forward", "pod/fakeghcr", ":8443")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	reader := bufio.NewReader(stdout)

	first, err := reader.ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		return fmt.Errorf("reading port-forward output: %w", err)
	}

	match := regexp.MustCompile(`127\.0\.0\.1:(\d+)`).FindStringSubmatch(first)
	if match == nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		return fmt.Errorf("unexpected port-forward output: %s", first)
	}

	fakeForward.cmd, fakeForward.port = cmd, match[1]

	// Keep reading so kubectl never gets a broken pipe.
	go func() { _, _ = io.Copy(io.Discard, reader) }()

	return nil
}

// dialFake connects to the fake registry, restarting the forward when the
// connection is refused or the forward does not exist yet.
func dialFake(ctx context.Context, network string) (net.Conn, error) {
	fakeForward.mutex.Lock()
	defer fakeForward.mutex.Unlock()

	var lastErr error

	for attempt := 0; attempt < 5; attempt++ {
		if fakeForward.cmd != nil {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+fakeForward.port)
			if err == nil {
				return conn, nil
			}

			lastErr = err
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if err := startFakeForward(); err != nil {
			lastErr = err

			time.Sleep(time.Second)
		}
	}

	return nil, fmt.Errorf("fake registry port-forward unavailable: %w", lastErr)
}

// seedOptions reaches the fake registry through a port-forward, with the CA and
// the PAT, so tests can put images in it and look at what is stored.
func seedOptions(t *testing.T) []remote.Option {
	t.Helper()

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(cluster.fakeCA))

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialFake(ctx, network)
		},
	}

	return []remote.Option{
		remote.WithAuth(&authn.Basic{Username: "e2e", Password: cluster.pat}),
		remote.WithTransport(transport),
	}
}

// seedImage stores a small random Linux image as ref ("soliddowant/private:1").
func seedImage(t *testing.T, ref string) {
	t.Helper()

	img, err := random.Image(1024, 1)
	require.NoError(t, err)

	config, err := img.ConfigFile()
	require.NoError(t, err)

	config.OS, config.Architecture = "linux", runtime.GOARCH
	img, err = mutate.ConfigFile(img, config)
	require.NoError(t, err)

	parsed, err := name.ParseReference(fakeRegistry + "/" + ref)
	require.NoError(t, err)
	require.NoError(t, remote.Write(parsed, img, seedOptions(t)...))
}

func upstreamDigest(t *testing.T, ref string) v1.Hash {
	t.Helper()

	parsed, err := name.ParseReference(fakeRegistry + "/" + ref)
	require.NoError(t, err)

	desc, err := remote.Head(parsed, seedOptions(t)...)
	require.NoError(t, err)

	return desc.Digest
}

// buildScript writes a one-file image and builds it as tag.
func buildScript(tag string) string {
	return fmt.Sprintf(`d=$(mktemp -d); cd "$d"; echo hello > hello; printf 'FROM scratch\nCOPY hello /hello\n' > Dockerfile; docker build -t %s .`, tag)
}

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

func TestBuildAndPushAllowedRepository(t *testing.T) {
	_, err := cluster.docker(t.Context(), buildScript("ghcr.io/soliddowant/app:1")+" && docker push ghcr.io/soliddowant/app:1")
	require.NoError(t, err)

	local, err := cluster.docker(t.Context(), `docker inspect --format '{{index .RepoDigests 0}}' ghcr.io/soliddowant/app:1`)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/soliddowant/app@"+upstreamDigest(t, "soliddowant/app:1").String(), strings.TrimSpace(local))
}

func TestPullPrivateAndPublicImages(t *testing.T) {
	seedImage(t, "soliddowant/private:1")
	seedImage(t, "soliddowant/public:1")

	before := len(fakeLog(t))

	_, err := cluster.docker(t.Context(), "docker pull ghcr.io/soliddowant/private:1")
	require.NoError(t, err)

	_, err = cluster.docker(t.Context(), `d=$(mktemp -d); cd "$d"; echo x > x; printf 'FROM ghcr.io/soliddowant/private:1\nCOPY x /x\n' > Dockerfile; docker build --no-cache -t local/derived:1 .`)
	require.NoError(t, err, "FROM of a private image")

	_, err = cluster.docker(t.Context(), "docker pull ghcr.io/soliddowant/public:1")
	require.NoError(t, err)

	for _, entry := range requestsFor(fakeLog(t)[before:], "soliddowant/public") {
		assert.NotEqual(t, "pat-token", entry.Credential, "%s %s", entry.Method, entry.Path)
	}
}

func TestPushOutsidePolicyIsRefused(t *testing.T) {
	_, err := cluster.docker(t.Context(), buildScript("ghcr.io/soliddowant/other:1"))
	require.NoError(t, err)

	before := len(fakeLog(t))
	_, err = cluster.docker(t.Context(), "docker push ghcr.io/soliddowant/other:1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "soliddowant/other")
	assert.Contains(t, err.Error(), "push")
	assert.Empty(t, requestsFor(fakeLog(t)[before:], "soliddowant/other"))
}

func TestPullOfPrivateImageOutsidePolicyFails(t *testing.T) {
	seedImage(t, "soliddowant/secret:1")

	before := len(fakeLog(t))
	_, err := cluster.docker(t.Context(), "docker pull ghcr.io/soliddowant/secret:1")
	require.Error(t, err)

	for _, entry := range requestsFor(fakeLog(t)[before:], "soliddowant/secret") {
		assert.NotEqual(t, "pat-token", entry.Credential, "%s %s", entry.Method, entry.Path)
		assert.NotEqual(t, http.StatusOK, entry.Status, "%s %s", entry.Method, entry.Path)
	}
}

// proxyMemoryBytes returns the largest resident memory of the proxy pods.
func proxyMemoryBytes(ctx context.Context) (float64, error) {
	pods, err := cluster.kubectl(ctx, "", "-n", proxyNamespace, "get", "pods", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return 0, err
	}

	var most float64

	for _, pod := range strings.Fields(pods) {
		metrics, err := cluster.kubectl(ctx, "", "get", "--raw", "/api/v1/namespaces/"+proxyNamespace+"/pods/"+pod+":8080/proxy/metrics")
		if err != nil {
			continue
		}

		match := regexp.MustCompile(`(?m)^process_resident_memory_bytes ([0-9.e+]+)$`).FindStringSubmatch(metrics)
		if match == nil {
			continue
		}

		value, err := strconv.ParseFloat(match[1], 64)
		if err == nil && value > most {
			most = value
		}
	}

	return most, nil
}

func TestPushOfTwoGiBLayerStaysUnderMemoryLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()

	var (
		mu   sync.Mutex
		peak float64
	)

	sampling, stop := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		for sampling.Err() == nil {
			if value, err := proxyMemoryBytes(sampling); err == nil {
				mu.Lock()
				peak = max(peak, value)
				mu.Unlock()
			}

			time.Sleep(2 * time.Second)
		}
	}()

	_, err := cluster.docker(ctx, `d=$(mktemp -d); cd "$d"; dd if=/dev/urandom of=big.bin bs=1M count=2048 2>/dev/null
printf 'FROM scratch\nCOPY big.bin /big.bin\n' > Dockerfile
docker build -t ghcr.io/soliddowant/big:1 . && docker push ghcr.io/soliddowant/big:1`)

	stop()
	<-done
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	require.NotZero(t, peak, "no memory sample was read")
	assert.Less(t, peak, float64(proxyMemory))
	_ = upstreamDigest(t, "soliddowant/big:1")
}

func TestPushesSucceedAfterAReplicaIsKilled(t *testing.T) {
	ctx := t.Context()

	_, err := cluster.docker(ctx, buildScript("local/restart:1"))
	require.NoError(t, err)

	push := func(tag string) error {
		_, err := cluster.docker(ctx, fmt.Sprintf("docker tag local/restart:1 ghcr.io/soliddowant/restart-1:%[1]s && docker push ghcr.io/soliddowant/restart-1:%[1]s", tag))

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

	_ = upstreamDigest(t, "soliddowant/restart-1:after-2")
}
