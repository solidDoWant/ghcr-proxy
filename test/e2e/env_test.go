//go:build e2e

// Package e2e runs real Docker clients in CI-like pods on a kind cluster
// against the proxy (installed from the chart), two fake upstream registries
// and Harbor.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	proxyNamespace  = "registrygate-system"
	harborNamespace = "harbor"
	proxyClusterIP  = "10.96.0.50" // the address in docs/admin/ci-pods.md
	clientLabel     = "registrygate-e2e-client"

	dockerCLIImage  = "mirror.gcr.io/library/docker:29.10.0-cli@sha256:34507eb5069d8be82c95dd3b6987d4250fd5fee49b82e2509b7d7a7030740d6d"
	dockerDindImage = "mirror.gcr.io/library/docker:29.10.0-dind@sha256:4bbc7223f81db790097c6dea42fb93e05b8fead4f0431fbe6ccaba6e8473201c"
)

// env is the cluster the tests share.
type env struct {
	kubeContext string
	proxyImage  string
	helperImage string
	fakeImage   string
	repoRoot    string
	// runID makes the names that outlive a run (Harbor robots) unique on a kept cluster.
	runID     string
	upstreams []*upstream
	// tenants are the CI namespaces.
	tenants []*tenant
	// harborURL is Harbor's core Service over plain HTTP, which the proxy talks to.
	harborURL string
	// harborExternalURL is the externalURL Harbor knows itself by: its token endpoint and Location headers name its host.
	harborExternalURL string
	// harborHostName is the name CI pods use for Harbor. The proxy stands in for it.
	harborHostName string
	harborCA       *ca
}

var cluster *env

func TestMain(m *testing.M) {
	kubeContext := os.Getenv("E2E_KUBE_CONTEXT")
	if kubeContext == "" {
		fmt.Fprintln(os.Stderr, "e2e: E2E_KUBE_CONTEXT is not set (no kind cluster); the e2e tests skip")
		os.Exit(0)
	}

	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: find the repository:", err)
		os.Exit(1)
	}

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}

	cluster = &env{
		kubeContext: kubeContext,
		proxyImage:  os.Getenv("E2E_PROXY_IMAGE"),
		helperImage: os.Getenv("E2E_HELPER_IMAGE"),
		fakeImage:   os.Getenv("E2E_FAKE_IMAGE"),
		repoRoot:    strings.TrimSpace(string(root)),
		runID:       hex.EncodeToString(suffix),
		harborURL:   "http://harbor-core." + harborNamespace + ".svc",

		harborExternalURL: "https://harbor.e2e.test",
		harborHostName:    "registry.e2e.test",
	}

	if err := cluster.setup(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: setup failed:", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// kubectl runs kubectl against the cluster and returns its standard output.
func (e *env) kubectl(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context", e.kubeContext}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("kubectl %s: %w\n%s%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}

	return stdout.String(), nil
}

func (e *env) apply(ctx context.Context, manifest string) error {
	_, err := e.kubectl(ctx, manifest, "apply", "-f", "-")

	return err
}

func (e *env) secret(ctx context.Context, namespace, name, secretType string, data map[string][]byte) error {
	secret := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": secretType,
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"data":     data,
	}

	raw, err := json.Marshal(secret)
	if err != nil {
		return err
	}

	return e.apply(ctx, string(raw))
}

func (e *env) setup(ctx context.Context) error {
	e.upstreams = newUpstreams()
	e.tenants = newTenants()

	var err error

	if e.harborCA, err = newCA(e.harborHostName); err != nil {
		return err
	}

	var manifest strings.Builder

	for _, name := range e.namespaceNames() {
		labels := ""
		if e.isTenant(name) {
			labels = "\n  labels: {" + clientLabel + ": \"true\"}"
		}

		fmt.Fprintf(&manifest, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s%s\n---\n", name, labels)
	}

	if err := e.apply(ctx, manifest.String()); err != nil {
		return err
	}

	// A cluster kept from an earlier run holds old certificates: start clean.
	if out, err := exec.CommandContext(ctx, "helm", "uninstall", "registrygate", "--kube-context", e.kubeContext, "--namespace", proxyNamespace, "--ignore-not-found", "--wait").CombinedOutput(); err != nil {
		return fmt.Errorf("helm uninstall: %w\n%s", err, out)
	}

	for _, tenant := range e.tenants {
		if _, err := e.kubectl(ctx, "", "-n", tenant.namespace, "delete", "pod", "build", "--ignore-not-found", "--wait"); err != nil {
			return err
		}
	}

	for _, fake := range e.upstreams {
		if err := fake.start(ctx, e); err != nil {
			return err
		}
	}

	if err := e.installProxy(ctx); err != nil {
		return err
	}

	if err := e.installHarbor(ctx); err != nil {
		return err
	}

	if err := e.createHarborObjects(ctx); err != nil {
		return err
	}

	if err := e.applyProxyObjects(ctx); err != nil {
		return err
	}

	return e.setupTenants(ctx)
}

func (e *env) namespaceNames() []string {
	names := []string{proxyNamespace, harborNamespace}
	for _, fake := range e.upstreams {
		names = append(names, fake.namespace)
	}

	for _, tenant := range e.tenants {
		names = append(names, tenant.namespace)
	}

	return names
}

func (e *env) isTenant(namespace string) bool {
	for _, tenant := range e.tenants {
		if tenant.namespace == namespace {
			return true
		}
	}

	return false
}

// installProxy installs the chart with no custom resources, and waits until every replica is ready.
func (e *env) installProxy(ctx context.Context) error {
	// The NetworkPolicy sees the API server's own endpoint, after Service translation.
	apiServer, err := e.kubectl(ctx, "", "get", "endpoints", "kubernetes", "-o", "jsonpath={.subsets[0].addresses[0].ip}")
	if err != nil {
		return err
	}

	tag := e.proxyImage[strings.LastIndex(e.proxyImage, ":")+1:]
	args := []string{"upgrade", "--install", "registrygate", filepath.Join(e.repoRoot, "deploy/charts/registrygate"),
		"--kube-context", e.kubeContext, "--namespace", proxyNamespace,
		"--set", "image.tag=" + tag, "--set", "image.pullPolicy=Never",
		"--set", "service.clusterIP=" + proxyClusterIP,
		"--set", "networkPolicy.enabled=true",
		"--set-string", "networkPolicy.clientNamespaceSelector.matchLabels." + clientLabel + "=true",
		"--set", "networkPolicy.apiServerCIDRs[0]=" + strings.TrimSpace(apiServer) + "/32",
		// NetworkPolicy sees the pod ports behind the Services of the fakes and Harbor's core, not the Service ports.
		"--set-json", `networkPolicy.extraEgress=[{"ports":[{"protocol":"TCP","port":8443},{"protocol":"TCP","port":8444},{"protocol":"TCP","port":8080}]}]`,
		"--set", "podDisruptionBudget.enabled=true",
		"--wait", "--timeout", "5m"}

	if out, err := exec.CommandContext(ctx, "helm", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("helm install: %w\n%s", err, out)
	}

	// After an upgrade the replaced pods are still shutting down. Later tests
	// count the pods, so wait until only the new ones are left.
	deadline := time.Now().Add(2 * time.Minute)

	for {
		pods, err := e.kubectl(ctx, "", "-n", proxyNamespace, "get", "pods", "-o", "name")
		if err != nil {
			return err
		}

		if len(strings.Fields(pods)) == 2 {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("the replaced proxy pods did not terminate:\n%s", pods)
		}

		time.Sleep(time.Second)
	}
}

// allHosts lists every name the proxy stands in for: those of the upstreams and Harbor's.
func (e *env) allHosts() []string {
	var hosts []string

	for _, fake := range e.upstreams {
		hosts = append(hosts, fake.hosts...)
	}

	return append(hosts, e.harborHostName)
}
