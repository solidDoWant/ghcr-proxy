//go:build e2e

// Package e2e runs real Docker clients in a CI-like pod on a kind cluster
// against the proxy (installed from the chart) and the fake GHCR.
package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const (
	proxyNamespace = "ghcr-proxy-system"
	fakeNamespace  = "fake-ghcr"
	ciNamespace    = "ci-foo"
	proxyClusterIP = "10.96.0.50" // the address in docs/admin/ci-pods.md
	storageHost    = "pkg-containers.githubusercontent.com"
	fakeRegistry   = "fakeghcr." + fakeNamespace + ".svc"
	fakeStorageSvc = "fakeghcr-storage"
	proxyMemory    = 256 << 20 // the chart's default memory limit
)

// env is the cluster the tests share.
type env struct {
	kubeContext string
	proxyImage  string
	helperImage string
	fakeImage   string
	repoRoot    string
	pat         string
	fakeCA      []byte
	proxyCA     []byte
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

	cluster = &env{
		kubeContext: kubeContext,
		proxyImage:  os.Getenv("E2E_PROXY_IMAGE"),
		helperImage: os.Getenv("E2E_HELPER_IMAGE"),
		fakeImage:   os.Getenv("E2E_FAKE_IMAGE"),
		repoRoot:    strings.TrimSpace(string(root)),
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

func (e *env) setup(ctx context.Context) error {
	pat := make([]byte, 16)
	if _, err := rand.Read(pat); err != nil {
		return err
	}

	e.pat = "ghp_e2e" + hex.EncodeToString(pat)

	if err := e.apply(ctx, namespaces()); err != nil {
		return err
	}

	// A cluster kept from an earlier run holds an old PAT and CA: start clean.
	if out, err := exec.CommandContext(ctx, "helm", "uninstall", "ghcr-proxy", "--kube-context", e.kubeContext, "--namespace", proxyNamespace, "--ignore-not-found", "--wait").CombinedOutput(); err != nil {
		return fmt.Errorf("helm uninstall: %w\n%s", err, out)
	}

	for namespace, pod := range map[string]string{fakeNamespace: "fakeghcr", ciNamespace: "build"} {
		if _, err := e.kubectl(ctx, "", "-n", namespace, "delete", "pod", pod, "--ignore-not-found", "--wait"); err != nil {
			return err
		}
	}

	if err := e.startFake(ctx); err != nil {
		return err
	}

	if err := e.installProxy(ctx); err != nil {
		return err
	}

	return e.setupCI(ctx)
}

func namespaces() string {
	var out strings.Builder
	for _, name := range []string{proxyNamespace, fakeNamespace, ciNamespace} {
		fmt.Fprintf(&out, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\n", name)
	}

	return out.String()
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

var pemBlock = regexp.MustCompile(`(?s)-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----\n`)

func (e *env) startFake(ctx context.Context) error {
	if err := e.secret(ctx, fakeNamespace, "pat", "Opaque", map[string][]byte{"token": []byte(e.pat)}); err != nil {
		return err
	}

	dnsNames := strings.Join([]string{fakeRegistry, fakeStorageSvc + "." + fakeNamespace + ".svc", storageHost}, ",")
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: fakeghcr
  namespace: %[1]s
  labels: {app: fakeghcr}
spec:
  containers:
    - name: fakeghcr
      image: %[2]s
      imagePullPolicy: Never
      args:
        - -registry-addr=:8443
        - -storage-addr=:8444
        - -admin-addr=:8445
        - -registry-host=%[3]s
        - -storage-host=%[4]s
        - -dns-names=%[5]s
        - -blob-dir=/blobs
        - -public=soliddowant/public
        - -pat-file=/pat/token
        - -ca-file=-
      volumeMounts:
        - {name: blobs, mountPath: /blobs}
        - {name: pat, mountPath: /pat, readOnly: true}
  volumes:
    - {name: blobs, emptyDir: {}}
    - {name: pat, secret: {secretName: pat}}
---
apiVersion: v1
kind: Service
metadata: {name: fakeghcr, namespace: %[1]s}
spec:
  selector: {app: fakeghcr}
  ports: [{port: 443, targetPort: 8443}]
---
apiVersion: v1
kind: Service
metadata: {name: %[6]s, namespace: %[1]s}
spec:
  selector: {app: fakeghcr}
  ports: [{port: 443, targetPort: 8444}]
---
apiVersion: v1
kind: Service
metadata: {name: fakeghcr-admin, namespace: %[1]s}
spec:
  selector: {app: fakeghcr}
  ports: [{port: 8445, targetPort: 8445}]
`, fakeNamespace, e.fakeImage, fakeRegistry, storageHost, dnsNames, fakeStorageSvc)

	if err := e.apply(ctx, manifest); err != nil {
		return err
	}

	if _, err := e.kubectl(ctx, "", "-n", fakeNamespace, "wait", "--for=condition=Ready", "pod/fakeghcr", "--timeout=180s"); err != nil {
		return err
	}

	var logs string

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		if logs, err = e.kubectl(ctx, "", "-n", fakeNamespace, "logs", "fakeghcr"); err != nil {
			return err
		}

		if e.fakeCA = []byte(pemBlock.FindString(logs)); len(e.fakeCA) > 0 {
			return nil
		}

		time.Sleep(time.Second)
	}

	return fmt.Errorf("the fake GHCR did not print its CA: %s", logs)
}

// newProxyCA makes a CA limited to the name ghcr.io and a server certificate
// for ghcr.io, as the admin guide asks.
func newProxyCA() (caPEM, certPEM, keyPEM []byte, err error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ghcr-proxy e2e CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		PermittedDNSDomains: []string{"ghcr.io"}, PermittedDNSDomainsCritical: true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, nil, err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}

	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "ghcr.io"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"ghcr.io"},
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, nil, nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

func (e *env) installProxy(ctx context.Context) error {
	caPEM, certPEM, keyPEM, err := newProxyCA()
	if err != nil {
		return err
	}

	e.proxyCA = caPEM

	if err := e.secret(ctx, proxyNamespace, "pat", "Opaque", map[string][]byte{"token": []byte(e.pat)}); err != nil {
		return err
	}

	if err := e.secret(ctx, proxyNamespace, "tls", "kubernetes.io/tls", map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM}); err != nil {
		return err
	}

	if err := e.secret(ctx, proxyNamespace, "upstream-ca", "Opaque", map[string][]byte{"ca.crt": e.fakeCA}); err != nil {
		return err
	}

	grants, err := json.Marshal([]map[string]any{{
		"namespaces": ciNamespace, "repositories": []string{"soliddowant/**"}, "actions": []string{"pull", "push", "delete"},
	}})
	if err != nil {
		return err
	}

	tag := e.proxyImage[strings.LastIndex(e.proxyImage, ":")+1:]
	cmd := exec.CommandContext(ctx, "helm", "upgrade", "--install", "ghcr-proxy", filepath.Join(e.repoRoot, "deploy/charts/ghcr-proxy"),
		"--kube-context", e.kubeContext, "--namespace", proxyNamespace,
		"--set", "image.tag="+tag, "--set", "image.pullPolicy=Never",
		"--set", "pat.existingSecret=pat", "--set", "tls.existingSecret=tls",
		"--set", "service.clusterIP="+proxyClusterIP,
		"--set", "upstream.url=https://"+fakeRegistry,
		"--set", "upstream.ca.existingSecret=upstream-ca",
		"--set-json", "grants="+string(grants),
		"--wait", "--timeout", "5m")

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("helm install: %w\n%s", err, out)
	}

	return nil
}

// ciPod reads the pod from docs/admin/ci-pods.md and adds only what the fake
// GHCR needs: its storage host.
func (e *env) ciPod(storageIP string) (corev1.Pod, error) {
	var pod corev1.Pod

	doc, err := os.ReadFile(filepath.Join(e.repoRoot, "docs/admin/ci-pods.md"))
	if err != nil {
		return pod, err
	}

	match := regexp.MustCompile("(?s)## Pod example.*?\n```yaml\n(.*?)\n```").FindSubmatch(doc)
	if match == nil {
		return pod, fmt.Errorf("docs/admin/ci-pods.md has no pod example")
	}

	if err := yaml.UnmarshalStrict(match[1], &pod); err != nil {
		return pod, err
	}

	// The guide leaves the images, the version and the command to the reader. The
	// tests fill them in and run the Docker commands themselves.
	pod.Namespace = ciNamespace
	images := map[string]string{
		"docker": "mirror.gcr.io/library/docker:29.9.3-cli@sha256:5a7af0dcb0e46c803efe9b45371dd21cb500e5b4453250394ac6f06d49b6e6f1",
		"dind":   "mirror.gcr.io/library/docker:29.9.3-dind@sha256:7bf70dcb85c822e3fa6f2d332143e474b99147b9d7b39102b0cfe8b04a7dd61b",
	}

	for index := range pod.Spec.Containers {
		container := &pod.Spec.Containers[index]
		container.Image = images[container.Name]

		if container.Name == "docker" {
			var setup []string

			for _, line := range strings.Split(container.Args[0], "\n") {
				if !strings.HasPrefix(line, "docker ") {
					setup = append(setup, line)
				}
			}

			container.Args = []string{strings.Join(setup, "\n") + "\nexec sleep infinity"}
		}
	}

	for index := range pod.Spec.Volumes {
		if image := pod.Spec.Volumes[index].Image; image != nil {
			image.Reference = e.helperImage
		}
	}

	pod.Spec.HostAliases = append(pod.Spec.HostAliases, corev1.HostAlias{IP: storageIP, Hostnames: []string{storageHost}})

	return pod, nil
}

func (e *env) setupCI(ctx context.Context) error {
	// Docker follows the blob redirect with the TLS settings of ghcr.io, so the
	// CA of the fake's storage host goes into the same file.
	if err := e.secret(ctx, ciNamespace, "ghcr-proxy-ca", "Opaque", map[string][]byte{"ca.crt": append(append([]byte{}, e.proxyCA...), e.fakeCA...)}); err != nil {
		return err
	}

	storageIP, err := e.kubectl(ctx, "", "-n", fakeNamespace, "get", "service", fakeStorageSvc, "-o", "jsonpath={.spec.clusterIP}")
	if err != nil {
		return err
	}

	pod, err := e.ciPod(strings.TrimSpace(storageIP))
	if err != nil {
		return err
	}

	raw, err := yaml.Marshal(pod)
	if err != nil {
		return err
	}

	policy := fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata: {name: builder, namespace: %[1]s}
---
apiVersion: ghcr-proxy.soliddowant.dev/v1alpha1
kind: GHCRAccessPolicy
metadata: {name: builder, namespace: %[1]s}
spec:
  serviceAccounts: [builder]
  rules:
    - repositories: [soliddowant/private, soliddowant/base]
      actions: [pull]
    - repositories: ["soliddowant/app", "soliddowant/app/**", soliddowant/big, "soliddowant/restart-*"]
      actions: [pull, push]
`, ciNamespace)
	if err := e.apply(ctx, policy); err != nil {
		return err
	}

	if err := e.apply(ctx, string(raw)); err != nil {
		return err
	}

	if _, err := e.kubectl(ctx, "", "-n", ciNamespace, "wait", "--for=condition=Ready", "pod/build", "--timeout=300s"); err != nil {
		return err
	}

	deadline := time.Now().Add(2 * time.Minute)

	for {
		_, err := e.docker(ctx, "docker info")
		if err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("the Docker daemon in the CI pod did not start: %w", err)
		}

		time.Sleep(2 * time.Second)
	}
}

// docker runs a shell script in the Docker CLI container of the CI pod.
func (e *env) docker(ctx context.Context, script string) (string, error) {
	return e.kubectl(ctx, "", "-n", ciNamespace, "exec", "build", "-c", "docker", "--", "sh", "-ec", script)
}
