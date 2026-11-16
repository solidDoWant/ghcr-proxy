//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	harborChart     = "oci://ghcr.io/home-operations/charts-mirror/harbor"
	harborVersion   = "1.19.4"
	harborURL       = "http://harbor." + harborNamespace + ".svc"
	harborAdmin     = "admin"
	harborPassword  = "Harbor12345"
	harborCredsName = "harbor-credentials"
)

// installHarbor installs Harbor from its chart and makes it reach the fake
// GHCR: the fake's blob redirects point to the storage host name, which the
// cluster DNS answers with the fake's storage Service.
func (e *env) installHarbor(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "helm", "upgrade", "--install", "harbor", harborChart, "--version", harborVersion,
		"--kube-context", e.kubeContext, "--namespace", harborNamespace, "--create-namespace",
		"-f", filepath.Join(e.repoRoot, "test/e2e/harbor-values.yaml"),
		"--set", "externalURL="+harborURL, "--wait", "--timeout", "10m")

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("helm install harbor: %w\n%s", err, out)
	}

	storageIP, err := e.kubectl(ctx, "", "-n", fakeNamespace, "get", "service", fakeStorageSvc, "-o", "jsonpath={.spec.clusterIP}")
	if err != nil {
		return err
	}

	corefile, err := e.kubectl(ctx, "", "-n", "kube-system", "get", "configmap", "coredns", "-o", "jsonpath={.data.Corefile}")
	if err != nil {
		return err
	}

	if !strings.Contains(corefile, storageHost) {
		hosts := fmt.Sprintf(".:53 {\n    hosts {\n        %s %s\n        fallthrough\n    }", strings.TrimSpace(storageIP), storageHost)

		patch, err := json.Marshal(map[string]any{"data": map[string]string{"Corefile": strings.Replace(corefile, ".:53 {", hosts, 1)}})
		if err != nil {
			return err
		}

		if _, err := e.kubectl(ctx, "", "-n", "kube-system", "patch", "configmap", "coredns", "--type", "merge", "-p", string(patch)); err != nil {
			return err
		}

		if _, err := e.kubectl(ctx, "", "-n", "kube-system", "rollout", "restart", "deployment/coredns"); err != nil {
			return err
		}

		if _, err := e.kubectl(ctx, "", "-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=120s"); err != nil {
			return err
		}
	}

	return nil
}

// harborAPI forwards a local port to Harbor and returns a function that sends
// an authenticated API request.
func harborAPI(t *testing.T) func(method, path string, body any) (int, []byte) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, "kubectl", "--context", cluster.kubeContext, "-n", harborNamespace, "port-forward", "service/harbor", ":80")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })

	reader := bufio.NewReader(stdout)
	first, err := reader.ReadString('\n')
	require.NoError(t, err)

	match := regexp.MustCompile(`127\.0\.0\.1:(\d+)`).FindStringSubmatch(first)
	require.NotNil(t, match, "port-forward output: %s", first)

	go func() { _, _ = io.Copy(io.Discard, reader) }()

	return func(method, path string, body any) (int, []byte) {
		t.Helper()

		var payload io.Reader

		if body != nil {
			raw, err := json.Marshal(body)
			require.NoError(t, err)

			payload = bytes.NewReader(raw)
		}

		req, err := http.NewRequestWithContext(t.Context(), method, "http://127.0.0.1:"+match[1]+"/api/v2.0"+path, payload)
		require.NoError(t, err)
		req.SetBasicAuth(harborAdmin, harborPassword)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		out, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		return resp.StatusCode, out
	}
}

// harborProject names the registry and the project of this run. It is unique,
// because the fake GHCR of a kept cluster has a new PAT and Harbor keeps the
// credentials of an older registry.
func harborProject() string { return "ghcr" + cluster.pat[len(cluster.pat)-8:] }

// createProxyCacheProject makes Harbor a proxy cache for the fake GHCR.
func createProxyCacheProject(t *testing.T, api func(string, string, any) (int, []byte)) {
	t.Helper()

	require.Eventually(t, func() bool {
		status, _ := api(http.MethodGet, "/ping", nil)

		return status == http.StatusOK
	}, 2*time.Minute, 2*time.Second)

	registry := map[string]any{
		"name": harborProject(), "type": "docker-registry", "url": "https://" + fakeRegistry, "insecure": true,
		"credential": map[string]any{"type": "basic", "access_key": "e2e", "access_secret": cluster.pat},
	}

	status, body := api(http.MethodPost, "/registries", registry)
	require.Contains(t, []int{http.StatusCreated, http.StatusConflict}, status, string(body))

	status, body = api(http.MethodGet, "/registries?q=name%3D"+harborProject(), nil)
	require.Equal(t, http.StatusOK, status, string(body))

	var registries []struct {
		ID int `json:"id"`
	}

	require.NoError(t, json.Unmarshal(body, &registries))
	require.Len(t, registries, 1)

	status, body = api(http.MethodPost, "/projects", map[string]any{"project_name": harborProject(), "public": true, "registry_id": registries[0].ID})
	require.Contains(t, []int{http.StatusCreated, http.StatusConflict}, status, string(body))
}

// blobReads counts the blob downloads of a repository the fake saw.
func blobReads(entries []fakeEntry, repo string) int {
	var count int

	for _, entry := range requestsFor(entries, repo) {
		if entry.Method == http.MethodGet && strings.Contains(entry.Path, "/blobs/") {
			count++
		}
	}

	return count
}

// This file sorts before docs_test.go on purpose: the docs walk-through removes
// the namespaces the shared setup made, so it must run last.
func TestSecondPullIsServedByHarborAsPolicyMirror(t *testing.T) {
	ctx := t.Context()

	require.NoError(t, cluster.installHarbor(ctx))

	api := harborAPI(t)
	createProxyCacheProject(t, api)

	require.NoError(t, cluster.secret(ctx, proxyNamespace, harborCredsName, "Opaque", map[string][]byte{"credentials": []byte(harborAdmin + ":" + harborPassword)}))

	// The other tests expect pulls to reach the fake GHCR: put the proxy back as it was.
	t.Cleanup(func() { require.NoError(t, cluster.installProxy(context.WithoutCancel(ctx))) })

	require.NoError(t, cluster.installProxy(ctx,
		"--set", "mirrors.policy.url="+harborURL,
		"--set", "mirrors.policy.pathPrefix="+harborProject(),
		"--set", "mirrors.policy.credentials.existingSecret="+harborCredsName,
		"--set", "mirrors.policy.fallbackToUpstream=false",
	))

	tag := fmt.Sprintf("mirror-%d", time.Now().UnixNano())
	ref := "soliddowant/base:" + tag
	seedImage(t, ref)

	_, err := cluster.docker(ctx, "docker pull ghcr.io/"+ref)
	require.NoError(t, err, "the first pull goes through Harbor to the fake GHCR")

	// Harbor records the repository after it has served the pull.
	require.Eventually(t, func() bool {
		status, body := api(http.MethodGet, "/projects/"+harborProject()+"/repositories", nil)

		return status == http.StatusOK && strings.Contains(string(body), harborProject()+"/soliddowant/base")
	}, time.Minute, time.Second, "Harbor must hold the image the pull fetched")

	_, err = cluster.docker(ctx, "docker rmi ghcr.io/"+ref)
	require.NoError(t, err)

	require.Positive(t, blobReads(fakeLog(t), "soliddowant/base"), "the first pull must have read the blobs from the fake GHCR")
	blobsBefore := blobReads(fakeLog(t), "soliddowant/base")

	_, err = cluster.docker(ctx, "docker pull ghcr.io/"+ref)
	require.NoError(t, err, "the second pull")

	entries := fakeLog(t)
	assert.Equal(t, blobsBefore, blobReads(entries, "soliddowant/base"), "Harbor must serve the second pull without a new blob download from the fake GHCR")
}
