//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	walkthroughNamespace = "registrygate"
	walkthroughTenant    = "ci-foo"
	walkthroughProject   = "ci-foo-ghcr"
)

// walkthroughPages are the admin pages in the order an admin follows them.
var walkthroughPages = []string{"install.md", "upstreams.md", "credentials.md", "ci-pods.md", "mirrors.md"}

var docBlock = regexp.MustCompile("(?s)<!-- e2e -->\n```sh\n(.*?)\n```")

// docCommands lists the shell blocks of a page that carry the e2e marker.
func docCommands(t *testing.T, page string) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(cluster.repoRoot, "docs/admin", page))
	require.NoError(t, err)

	var blocks []string
	for _, match := range docBlock.FindAllStringSubmatch(string(raw), -1) {
		blocks = append(blocks, match[1])
	}

	require.NotEmpty(t, blocks, page)

	return blocks
}

// walkthrough runs the commands of the admin docs on a cluster that has
// nothing of registrygate in it. The harness changes them only as the design
// allows: the chart comes from this repository, the UpstreamRegistry and
// RegistryMirror objects point to the in-cluster fakes and Harbor, and the
// fakes' and Harbor's CAs are trusted.
type walkthrough struct {
	t          *testing.T
	workdir    string
	kubeconfig string
	env        []string
}

func (w *walkthrough) run(ctx context.Context, name, script string) {
	w.t.Helper()

	cmd := exec.CommandContext(ctx, "bash", "-ec", script)
	cmd.Dir = w.workdir
	cmd.Env = append(os.Environ(), w.env...)

	var out bytes.Buffer

	cmd.Stdout, cmd.Stderr = &out, &out

	if err := cmd.Run(); err != nil {
		w.t.Fatalf("%s:\n%s\noutput:\n%s\nerror: %v\ndiagnostics:\n%s", name, script, out.String(), err, cluster.podDiagnostics(ctx, walkthroughTenant, "pod/build"))
	}
}

// transform applies the substitutions the harness is allowed to make.
func (w *walkthrough) transform(script string) string {
	chart := filepath.Join(cluster.repoRoot, "deploy/charts/registrygate")
	tag := cluster.proxyImage[strings.LastIndex(cluster.proxyImage, ":")+1:]
	script = strings.ReplaceAll(script, "oci://ghcr.io/soliddowant/charts/registrygate", chart+" --set image.tag="+tag+" --set image.pullPolicy=Never")

	for _, fake := range cluster.upstreams {
		pattern := regexp.MustCompile(`(?m)^(\s*)url: https://` + regexp.QuoteMeta(fake.host) + `$`)
		script = pattern.ReplaceAllString(script, "${1}url: https://"+fake.registryHost()+"\n${1}caSecretRef:\n${1}  name: "+fake.objectName()+"-ca")
	}

	script = strings.ReplaceAll(script, "harbor.example.com", strings.TrimPrefix(cluster.harborTLSURL, "https://"))
	script = strings.ReplaceAll(script, "create secret generic harbor-ci-foo", "create secret generic harbor-ci-foo --from-file=ca.crt=./harbor-ca.crt")
	script = strings.ReplaceAll(script, "  secretRef:\n    name: harbor-ci-foo\n", "  secretRef:\n    name: harbor-ci-foo\n  caSecretRef:\n    name: harbor-ci-foo\n")

	return script
}

// resetCluster removes what an earlier test or run left of the proxy and the
// objects of the guide, so that the walk-through starts from nothing. Fakes and
// Harbor stay: the harness is allowed to install them.
func resetCluster(ctx context.Context, t *testing.T) {
	t.Helper()

	for release, namespace := range map[string]string{"registrygate": proxyNamespace, "cert-manager": "cert-manager"} {
		out, err := exec.CommandContext(ctx, "helm", "uninstall", release, "--kube-context", cluster.kubeContext, "--namespace", namespace, "--ignore-not-found", "--wait").CombinedOutput()
		require.NoError(t, err, string(out))
	}

	out, err := exec.CommandContext(ctx, "helm", "uninstall", "registrygate", "--kube-context", cluster.kubeContext, "--namespace", walkthroughNamespace, "--ignore-not-found", "--wait").CombinedOutput()
	require.NoError(t, err, string(out))

	// cert-manager's chart keeps its CRDs when it is uninstalled.
	crds, err := cluster.kubectl(ctx, "", "get", "crd", "-o", "name")
	require.NoError(t, err)

	var stale []string

	for _, crd := range strings.Fields(crds) {
		if strings.HasSuffix(crd, ".cert-manager.io") || strings.HasSuffix(crd, ".acme.cert-manager.io") {
			stale = append(stale, crd)
		}
	}

	if len(stale) > 0 {
		_, err = cluster.kubectl(ctx, "", append([]string{"delete", "--ignore-not-found"}, stale...)...)
		require.NoError(t, err)
	}

	namespaces := []string{proxyNamespace, walkthroughNamespace, walkthroughTenant, "cert-manager"}
	for _, tenant := range cluster.tenants {
		namespaces = append(namespaces, tenant.namespace)
	}

	_, err = cluster.kubectl(ctx, "", append([]string{"delete", "namespace", "--ignore-not-found", "--wait"}, namespaces...)...)
	require.NoError(t, err)
}

// createWalkthroughHarborProject makes the proxy-cache project of the guide's
// mirror, with a registry endpoint that has no credential and a robot that can
// pull. It returns the robot.
func createWalkthroughHarborProject(ctx context.Context, t *testing.T, fake *upstream) robot {
	t.Helper()

	api, err := sharedHarborClient()
	require.NoError(t, err)

	// A kept cluster still has the project: delete what it cached, then the project.
	status, _, err := api.do(ctx, http.MethodGet, "/projects/"+walkthroughProject+"/repositories?page_size=100", nil)
	require.NoError(t, err)

	if status == http.StatusOK {
		for _, repo := range harborRepositories(t, walkthroughProject) {
			// Harbor wants a slash in a repository name encoded twice.
			deleteHarbor(ctx, t, api, "/projects/"+walkthroughProject+"/repositories/"+url.PathEscape(url.PathEscape(repo)))
		}

		deleteHarbor(ctx, t, api, "/projects/"+walkthroughProject)
	}

	registryID := harborRegistryID(ctx, t, api, "fake-"+fake.key)

	status, body, err := api.do(ctx, http.MethodPost, "/projects", map[string]any{"project_name": walkthroughProject, "public": false, "registry_id": registryID})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status, string(body))

	return createHarborRobot(ctx, t, api, walkthroughProject, "walkthrough-"+cluster.runID)
}

func deleteHarbor(ctx context.Context, t *testing.T, api *harborClient, path string) {
	t.Helper()

	status, body, err := api.do(ctx, http.MethodDelete, path, nil)
	require.NoError(t, err)
	require.Contains(t, []int{http.StatusOK, http.StatusNotFound}, status, string(body))
}

// harborRegistryID finds the id of a registry endpoint by name.
func harborRegistryID(ctx context.Context, t *testing.T, api *harborClient, name string) int {
	t.Helper()

	status, body, err := api.do(ctx, http.MethodGet, "/registries?q=name%3D"+name, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, string(body))

	var found []struct {
		ID int `json:"id"`
	}

	require.NoError(t, json.Unmarshal(body, &found))
	require.Len(t, found, 1, string(body))

	return found[0].ID
}

// createHarborRobot makes a robot that can pull from a project.
func createHarborRobot(ctx context.Context, t *testing.T, api *harborClient, project, name string) robot {
	t.Helper()

	status, body, err := api.do(ctx, http.MethodPost, "/robots", map[string]any{
		"name": name, "level": "project", "duration": -1,
		"permissions": []map[string]any{{"kind": "project", "namespace": project, "access": []map[string]string{{"resource": "repository", "action": "pull"}}}},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status, string(body))

	var created struct {
		Name   string `json:"name"`
		Secret string `json:"secret"`
	}

	require.NoError(t, json.Unmarshal(body, &created))

	return robot{username: created.Name, password: created.Secret}
}

func TestAdminDocsGetToAPushAndAMirroredPull(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()

	resetCluster(ctx, t)

	fakeA := cluster.upstreams[0]
	robot := createWalkthroughHarborProject(ctx, t, fakeA)

	// The public image that the mirror caches. The test puts it in the fake upstream.
	fakeA.seedImage(t, "soliddowant/public:latest")

	workdir := t.TempDir()
	kubeconfig, err := exec.CommandContext(ctx, "kubectl", "config", "view", "--raw", "--minify", "--context", cluster.kubeContext).Output()
	require.NoError(t, err)

	kubeconfigPath := filepath.Join(workdir, "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfigPath, kubeconfig, 0o600))
	// The guide reads the PAT from a file and the Harbor CA is trusted by the proxy.
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "pat.txt"), []byte(fakeA.pat), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "harbor-ca.crt"), cluster.harborCA, 0o600))

	walk := &walkthrough{t: t, workdir: workdir, kubeconfig: kubeconfigPath, env: []string{
		"KUBECONFIG=" + kubeconfigPath,
		"GITHUB_USER=e2e",
		"DOCKER_CLI_IMAGE=" + dockerCLIImage,
		"DOCKER_DIND_IMAGE=" + dockerDindImage,
		"HELPER_IMAGE=" + cluster.helperImage,
		"HARBOR_USER=" + robot.username,
		"HARBOR_PASSWORD=" + robot.password,
	}}

	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Minute)
		defer stop()

		for release, namespace := range map[string]string{"registrygate": walkthroughNamespace, "cert-manager": "cert-manager"} {
			_ = exec.CommandContext(cleanup, "helm", "uninstall", release, "--kube-context", cluster.kubeContext, "--namespace", namespace, "--ignore-not-found", "--wait").Run()
		}

		_, _ = cluster.kubectl(cleanup, "", "delete", "namespace", "--ignore-not-found", walkthroughNamespace, walkthroughTenant, "cert-manager")
	})

	for _, page := range walkthroughPages {
		for index, block := range docCommands(t, page) {
			walk.run(ctx, fmt.Sprintf("docs/admin/%s block %d", page, index+1), walk.transform(block))

			// The upstreams' CAs are the one thing the guide cannot have: it
			// names real hosts. They must exist before an UpstreamRegistry
			// that names them.
			if strings.Contains(block, "kubectl create namespace registrygate") {
				for _, fake := range cluster.upstreams {
					require.NoError(t, cluster.secret(ctx, walkthroughNamespace, fake.objectName()+"-ca", "Opaque", map[string][]byte{"ca.crt": fake.fakeCA}))
				}
			}
		}
	}

	// The push reached upstream A with the PAT, and it holds the image.
	assert.NotEmpty(t, fakeA.digest(t, "soliddowant/foo:latest"))

	var pushed bool

	for _, entry := range requestsFor(fakeA.log(t), "soliddowant/foo") {
		pushed = pushed || (entry.Method == http.MethodPut && strings.Contains(entry.Path, "/manifests/") && entry.Status < 300 && entry.Credential == "pat-token")
	}

	assert.True(t, pushed, "a manifest PUT with the PAT's token")

	// The mirrored pull went through Harbor: it cached the image.
	// Harbor lists a repository a little after the pull ends.
	assert.Eventually(t, func() bool {
		return slices.Contains(harborRepositories(t, walkthroughProject), "soliddowant/public")
	}, time.Minute, 2*time.Second, "Harbor caches the image the CI pod pulled")
}
