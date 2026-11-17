//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

// upstream is a fake registry in its own namespace, together with what the
// proxy needs to stand in for one host.
type upstream struct {
	// key is the suffix of the object and project names ("a" or "b").
	key string
	// host is the main name the proxy stands in for; hosts is every name, host first.
	host      string
	hosts     []string
	namespace string
	// mirrorRepo is the repository glob the tenants' mirror rules cover.
	mirrorRepo string
	// auth, uploadPath and deleteMode are the fake's flags.
	auth, uploadPath, deleteMode string
	storageHost                  string

	pat       string
	fakeCA    []byte
	servingCA *ca
}

const (
	fakePod        = "fakeregistry"
	fakeStorageSvc = "fakeregistry-storage"
)

func newUpstreams() []*upstream {
	upstreams := []*upstream{
		// Upstream A behaves like ghcr.io.
		{key: "a", host: "ghcr.io", namespace: "fake-a", auth: "bearer", uploadPath: "upload", deleteMode: "unsupported", storageHost: "pkg-containers.githubusercontent.com", mirrorRepo: "mirrored/**"},
		{key: "b", host: "quay.io", namespace: "fake-b", auth: "basic", uploadPath: "uploads", deleteMode: "supported", storageHost: "cdn01.quay.io", mirrorRepo: "mirrored/**"},
		// Upstream C stands in for Docker Hub, which clients know by three names. It is not Docker Hub: it proves the naming only.
		{key: "c", host: "registry-1.docker.io", hosts: []string{"registry-1.docker.io", "docker.io", "index.docker.io"}, namespace: "fake-c", auth: "bearer", uploadPath: "uploads", deleteMode: "supported", storageHost: "production.cloudflare.docker.com", mirrorRepo: "library/**"},
	}

	for _, fake := range upstreams {
		if fake.hosts == nil {
			fake.hosts = []string{fake.host}
		}
	}

	return upstreams
}

// registryHost is the in-cluster name of the fake.
func (u *upstream) registryHost() string { return fakePod + "." + u.namespace + ".svc" }

func (u *upstream) objectName() string { return "upstream-" + u.key }

// storageIP is the ClusterIP of the Service for the blob redirects.
func (u *upstream) storageIP(ctx context.Context, e *env) (string, error) {
	ip, err := e.kubectl(ctx, "", "-n", u.namespace, "get", "service", fakeStorageSvc, "-o", "jsonpath={.spec.clusterIP}")

	return strings.TrimSpace(ip), err
}

var pemBlock = regexp.MustCompile(`(?s)-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----\n`)

func (u *upstream) start(ctx context.Context, e *env) error {
	pat := make([]byte, 16)
	if _, err := rand.Read(pat); err != nil {
		return err
	}

	u.pat = "ghp_e2e" + hex.EncodeToString(pat)

	if _, err := e.kubectl(ctx, "", "-n", u.namespace, "delete", "pod", fakePod, "--ignore-not-found", "--wait"); err != nil {
		return err
	}

	if err := e.secret(ctx, u.namespace, "pat", "Opaque", map[string][]byte{"token": []byte(u.pat)}); err != nil {
		return err
	}

	dnsNames := strings.Join([]string{u.registryHost(), fakeStorageSvc + "." + u.namespace + ".svc", u.storageHost}, ",")
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %[2]s
  namespace: %[1]s
  labels: {app: %[2]s}
spec:
  containers:
    - name: fakeregistry
      image: %[3]s
      imagePullPolicy: Never
      args:
        - -registry-addr=:8443
        - -storage-addr=:8444
        - -admin-addr=:8445
        - -registry-host=%[4]s
        - -storage-host=%[5]s
        - -dns-names=%[6]s
        - -blob-dir=/blobs
        - -public=soliddowant/public,library/alpine,%[11]s,%[12]s
        - -pat-file=/pat/token
        - -ca-file=-
        - -auth=%[8]s
        - -upload-path=%[9]s
        - -delete=%[10]s
      volumeMounts:
        - {name: blobs, mountPath: /blobs}
        - {name: pat, mountPath: /pat, readOnly: true}
  volumes:
    - {name: blobs, emptyDir: {}}
    - {name: pat, secret: {secretName: pat}}
---
apiVersion: v1
kind: Service
metadata: {name: %[2]s, namespace: %[1]s}
spec:
  selector: {app: %[2]s}
  ports: [{port: 443, targetPort: 8443}]
---
apiVersion: v1
kind: Service
metadata: {name: %[7]s, namespace: %[1]s}
spec:
  selector: {app: %[2]s}
  ports: [{port: 443, targetPort: 8444}]
---
apiVersion: v1
kind: Service
metadata: {name: %[2]s-admin, namespace: %[1]s}
spec:
  selector: {app: %[2]s}
  ports: [{port: 8445, targetPort: 8445}]
`, u.namespace, fakePod, e.fakeImage, u.registryHost(), u.storageHost, dnsNames, fakeStorageSvc, u.auth, u.uploadPath, u.deleteMode, mirroredRepo("base"), mirroredRepo("build"))

	if err := e.apply(ctx, manifest); err != nil {
		return err
	}

	if _, err := e.kubectl(ctx, "", "-n", u.namespace, "wait", "--for=condition=Ready", "pod/"+fakePod, "--timeout=180s"); err != nil {
		return err
	}

	var logs string

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		if logs, err = e.kubectl(ctx, "", "-n", u.namespace, "logs", fakePod); err != nil {
			return err
		}

		if u.fakeCA = []byte(pemBlock.FindString(logs)); len(u.fakeCA) > 0 {
			break
		}

		time.Sleep(time.Second)
	}

	if len(u.fakeCA) == 0 {
		return fmt.Errorf("the fake registry in %s did not print its CA: %s", u.namespace, logs)
	}

	var err error

	u.servingCA, err = newCA(u.hosts...)

	return err
}

// fakeEntry is one request in a fake registry's log.
type fakeEntry struct {
	Server     string
	Method     string
	Path       string
	Credential string
	Status     int
}

// log is the fake's request log.
func (u *upstream) log(t *testing.T) []fakeEntry {
	t.Helper()

	raw, err := cluster.kubectl(t.Context(), "", "get", "--raw", "/api/v1/namespaces/"+u.namespace+"/services/"+fakePod+"-admin:8445/proxy/log")
	require.NoError(t, err)

	var entries []fakeEntry
	require.NoError(t, json.Unmarshal([]byte(raw), &entries))

	return entries
}

// requestsFor lists the registry requests a fake saw for a repository.
func requestsFor(entries []fakeEntry, repo string) []fakeEntry {
	var out []fakeEntry

	for _, entry := range entries {
		if entry.Server == "registry" && strings.HasPrefix(entry.Path, "/v2/"+repo+"/") {
			out = append(out, entry)
		}
	}

	return out
}

// seedOptions reaches the fake registry through a port-forward that lasts
// until the test ends, with the CA and the PAT, so tests can put images in it
// and look at what is stored.
func (u *upstream) seedOptions(t *testing.T) []remote.Option {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, "kubectl", "--context", cluster.kubeContext, "-n", u.namespace, "port-forward", "pod/"+fakePod, ":8443")
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

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(u.fakeCA))

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+match[1])
		},
	}

	return []remote.Option{
		remote.WithAuth(&authn.Basic{Username: "e2e", Password: u.pat}),
		remote.WithTransport(transport),
	}
}

// seedImage stores a small random Linux image as ref ("mirrored/base:1").
func (u *upstream) seedImage(t *testing.T, ref string) {
	t.Helper()

	img, err := random.Image(1024, 1)
	require.NoError(t, err)

	config, err := img.ConfigFile()
	require.NoError(t, err)

	config.OS, config.Architecture = "linux", runtime.GOARCH
	img, err = mutate.ConfigFile(img, config)
	require.NoError(t, err)

	parsed, err := name.ParseReference(u.registryHost() + "/" + ref)
	require.NoError(t, err)
	require.NoError(t, remote.Write(parsed, img, u.seedOptions(t)...))
}

func (u *upstream) digest(t *testing.T, ref string) v1.Hash {
	t.Helper()

	parsed, err := name.ParseReference(u.registryHost() + "/" + ref)
	require.NoError(t, err)

	desc, err := remote.Head(parsed, u.seedOptions(t)...)
	require.NoError(t, err)

	return desc.Digest
}

// mirroredRepo names a repository the mirror rules cover. Harbor keeps what it
// cached on a kept cluster, so each run uses its own names.
func mirroredRepo(purpose string) string { return "mirrored/" + purpose + "-" + cluster.runID }
