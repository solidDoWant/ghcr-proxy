//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	harborChart    = "oci://ghcr.io/home-operations/charts-mirror/harbor"
	harborVersion  = "1.19.4"
	harborAdmin    = "admin"
	harborPassword = "Harbor12345"
)

// installHarbor installs Harbor from its chart and makes it reach the fake
// registries: the fakes' blob redirects point to their storage host names,
// which the cluster DNS answers with the fakes' storage Services.
func (e *env) installHarbor(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "helm", "upgrade", "--install", "harbor", harborChart, "--version", harborVersion,
		"--kube-context", e.kubeContext, "--namespace", harborNamespace,
		"-f", filepath.Join(e.repoRoot, "test/e2e/harbor-values.yaml"),
		"--set", "externalURL="+e.harborExternalURL, "--wait", "--timeout", "10m")

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("helm install harbor: %w\n%s", err, out)
	}

	var hosts strings.Builder

	for _, fake := range e.upstreams {
		ip, err := fake.storageIP(ctx, e)
		if err != nil {
			return err
		}

		fmt.Fprintf(&hosts, "        %s %s\n", ip, fake.storageHost)
	}

	corefile, err := e.kubectl(ctx, "", "-n", "kube-system", "get", "configmap", "coredns", "-o", "jsonpath={.data.Corefile}")
	if err != nil {
		return err
	}

	// A kept cluster has old addresses in the block: take it out first.
	corefile = regexp.MustCompile(`(?s)\.:53 \{\n    hosts \{\n.*?\n    \}\n`).ReplaceAllString(corefile, ".:53 {\n")
	corefile = strings.Replace(corefile, ".:53 {\n", ".:53 {\n    hosts {\n"+hosts.String()+"        fallthrough\n    }\n", 1)

	patch, err := json.Marshal(map[string]any{"data": map[string]string{"Corefile": corefile}})
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

	return nil
}

// harborClient sends authenticated Harbor API requests through a port-forward.
type harborClient struct {
	port string
	http *http.Client
}

// newHarborClient forwards a local port to Harbor until the context ends.
func newHarborClient(ctx context.Context, kubeContext string) (*harborClient, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "--context", kubeContext, "-n", harborNamespace, "port-forward", "service/harbor", ":443")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go func() { _ = cmd.Wait() }()

	reader := bufio.NewReader(stdout)

	first, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading port-forward output: %w", err)
	}

	match := regexp.MustCompile(`127\.0\.0\.1:(\d+)`).FindStringSubmatch(first)
	if match == nil {
		return nil, fmt.Errorf("unexpected port-forward output: %s", first)
	}

	go func() { _, _ = io.Copy(io.Discard, reader) }()

	return &harborClient{
		port: match[1],
		// The certificate names harbor.harbor.svc, not the loopback address; tests use the CA through the proxy.
		http: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}, Timeout: time.Minute}, //nolint:gosec // loopback port-forward to the test's own Harbor
	}, nil
}

func (h *harborClient) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var payload io.Reader

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}

		payload = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, "https://127.0.0.1:"+h.port+"/api/v2.0"+path, payload)
	if err != nil {
		return 0, nil, err
	}

	req.SetBasicAuth(harborAdmin, harborPassword)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.http.Do(req)
	if err != nil {
		return 0, nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)

	return resp.StatusCode, out, err
}

// harborPushProject is the project a namespace's CI pod pushes to.
func harborPushProject(namespace string) string { return namespace + "-push" }

// harborProject is the proxy-cache project of a namespace for an upstream.
func harborProject(namespace string, fake *upstream) string { return namespace + "-" + fake.key }

// createHarborObjects makes a registry endpoint for each upstream (no
// credential), a public proxy-cache project for each namespace and upstream,
// and for each namespace a project with a push robot, for pushes through the
// proxy.
func (e *env) createHarborObjects(ctx context.Context) error {
	forwardCtx, stop := context.WithCancel(ctx)
	defer stop()

	api, err := newHarborClient(forwardCtx, e.kubeContext)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(2 * time.Minute)

	for {
		status, _, err := api.do(ctx, http.MethodGet, "/ping", nil)
		if err == nil && status == http.StatusOK {
			break
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("harbor did not answer its ping: %d %v", status, err)
		}

		time.Sleep(2 * time.Second)
	}

	registryIDs := map[string]int{}

	for _, fake := range e.upstreams {
		name := "fake-" + fake.key
		registry := map[string]any{"name": name, "type": "docker-registry", "url": "https://" + fake.registryHost(), "insecure": true}

		status, body, err := api.do(ctx, http.MethodPost, "/registries", registry)
		if err != nil || (status != http.StatusCreated && status != http.StatusConflict) {
			return fmt.Errorf("create registry %s: %d %s %v", name, status, body, err)
		}

		status, body, err = api.do(ctx, http.MethodGet, "/registries?q=name%3D"+name, nil)
		if err != nil || status != http.StatusOK {
			return fmt.Errorf("find registry %s: %d %s %v", name, status, body, err)
		}

		var found []struct {
			ID int `json:"id"`
		}

		if err := json.Unmarshal(body, &found); err != nil || len(found) != 1 {
			return fmt.Errorf("find registry %s: %s %v", name, body, err)
		}

		registryIDs[fake.key] = found[0].ID
	}

	for _, tenant := range e.tenants {
		for _, fake := range e.upstreams {
			project := harborProject(tenant.namespace, fake)

			// The project is public so a mirror needs no credential: the namespace holds none of Harbor's.
			status, body, err := api.do(ctx, http.MethodPost, "/projects", map[string]any{"project_name": project, "public": true, "registry_id": registryIDs[fake.key]})
			if err != nil || (status != http.StatusCreated && status != http.StatusConflict) {
				return fmt.Errorf("create project %s: %d %s %v", project, status, body, err)
			}
		}

		push := harborPushProject(tenant.namespace)

		status, body, err := api.do(ctx, http.MethodPost, "/projects", map[string]any{"project_name": push, "public": false})
		if err != nil || (status != http.StatusCreated && status != http.StatusConflict) {
			return fmt.Errorf("create project %s: %d %s %v", push, status, body, err)
		}

		status, body, err = api.do(ctx, http.MethodPost, "/robots", map[string]any{
			"name": "e2e-" + e.runID, "level": "project", "duration": -1,
			"permissions": []map[string]any{{"kind": "project", "namespace": push, "access": []map[string]string{
				{"resource": "repository", "action": "pull"}, {"resource": "repository", "action": "push"}, {"resource": "repository", "action": "delete"},
			}}},
		})
		if err != nil || status != http.StatusCreated {
			return fmt.Errorf("create robot for %s: %d %s %v", push, status, body, err)
		}

		var created struct {
			Name   string `json:"name"`
			Secret string `json:"secret"`
		}

		if err := json.Unmarshal(body, &created); err != nil {
			return err
		}

		tenant.pushRobot = robot{username: created.Name, password: created.Secret}
	}

	return nil
}

var sharedHarbor struct {
	sync.Mutex
	client *harborClient
}

// sharedHarborClient forwards one port for the whole run: tests poll Harbor
// and a forward for each poll would be slow and flaky.
func sharedHarborClient() (*harborClient, error) {
	sharedHarbor.Lock()
	defer sharedHarbor.Unlock()

	if sharedHarbor.client == nil {
		client, err := newHarborClient(context.Background(), cluster.kubeContext)
		if err != nil {
			return nil, err
		}

		sharedHarbor.client = client
	}

	return sharedHarbor.client, nil
}

// harborRepositories lists the repositories a Harbor project holds.
func harborRepositories(t *testing.T, project string) []string {
	t.Helper()

	api, err := sharedHarborClient()
	require.NoError(t, err)

	status, body, err := api.do(t.Context(), http.MethodGet, "/projects/"+project+"/repositories?page_size=100", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, string(body))

	var repos []struct {
		Name string `json:"name"`
	}

	require.NoError(t, json.Unmarshal(body, &repos))

	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		names = append(names, strings.TrimPrefix(repo.Name, project+"/"))
	}

	return names
}
