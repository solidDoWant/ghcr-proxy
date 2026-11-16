//go:build live

package live

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/stretchr/testify/require"
)

// The probes ask ghcr.io itself, with the PAT, the questions the delete design
// depends on. They act only on repositories under the run prefix and print
// status codes, registry error codes and expires_in, never a token.

// tokenAnswer is what GHCR's token endpoint answered for one scope.
type tokenAnswer struct {
	status    int
	token     string
	expiresIn int
}

// probeClient talks to ghcr.io only and never follows redirects, so neither
// the PAT nor a registry token goes to another host.
func probeClient() *http.Client {
	return &http.Client{
		Timeout:       time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// requestToken asks GHCR's token endpoint for repository:<repo>:<actions>
// with the PAT.
func (e *liveEnv) requestToken(t *testing.T, repo, actions string) tokenAnswer {
	t.Helper()
	require.True(t, e.inRunPrefix(repo), "probe repository %q is outside the run prefix", repo)

	query := url.Values{"service": {"ghcr.io"}, "scope": {"repository:" + repo + ":" + actions}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, registryURL+"/token?"+query.Encode(), nil)
	require.NoError(t, err)
	req.SetBasicAuth(e.owner, e.pat)

	resp, err := probeClient().Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	answer := tokenAnswer{status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		return answer
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	answer.token, answer.expiresIn = body.Token, body.ExpiresIn
	if answer.token == "" {
		answer.token = body.AccessToken
	}

	return answer
}

// registryAnswer is GHCR's answer to one registry request: the status, the
// registry error codes and the scope its challenge asks for, if any.
type registryAnswer struct {
	status         int
	codes          []string
	challengeScope string
}

// call sends method to /v2/<repo>/<kind>/<reference> on ghcr.io with token.
func (e *liveEnv) call(t *testing.T, method, token, repo, kind, reference string) registryAnswer {
	t.Helper()
	require.True(t, e.inRunPrefix(repo), "probe repository %q is outside the run prefix", repo)

	req, err := http.NewRequestWithContext(t.Context(), method, registryURL+"/v2/"+repo+"/"+kind+"/"+reference, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)

	if kind == "manifests" {
		req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	}

	resp, err := probeClient().Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	answer := registryAnswer{status: resp.StatusCode}

	if challenge := resp.Header.Get("WWW-Authenticate"); challenge != "" {
		if _, after, found := strings.Cut(challenge, `scope="`); found {
			answer.challengeScope, _, _ = strings.Cut(after, `"`)
		}
	}

	var registryErr *transport.Error
	if errors.As(transport.CheckError(resp, http.StatusOK, http.StatusAccepted), &registryErr) {
		for _, diagnostic := range registryErr.Errors {
			answer.codes = append(answer.codes, string(diagnostic.Code))
		}
	}

	_, _ = io.Copy(io.Discard, resp.Body)

	return answer
}

func (e *liveEnv) logAnswer(t *testing.T, question string, answer registryAnswer) {
	t.Helper()
	t.Logf("%s: status %d, error codes %v, challenge scope %q", question, answer.status, answer.codes, answer.challengeScope)
}

func TestGHCRDeleteBehaviour(t *testing.T) {
	env := requireLive(t)
	repo := env.repo("probe")

	// Every probe gets its own image, so a delete that removes more than its
	// tag can't change another probe's answer.
	scopeTags := map[string]string{"delete": "scope-delete", "*": "scope-star", "pull,push,delete": "scope-pull-push-delete"}
	for _, tag := range scopeTags {
		env.pushThroughProxy(t, repo, tag)
	}

	imageA := env.pushThroughProxy(t, repo, "by-tag")
	blobImage := env.pushThroughProxy(t, repo, "blob")
	imageB := env.pushThroughProxy(t, repo, "by-digest")

	var deleteToken string

	t.Run("token endpoint scopes for delete", func(t *testing.T) {
		for _, actions := range []string{"delete", "*", "pull,push,delete"} {
			answer := env.requestToken(t, repo, actions)
			t.Logf("token for actions %q: status %d, expires_in %d", actions, answer.status, answer.expiresIn)

			if answer.status != http.StatusOK {
				continue
			}

			if actions == "pull,push,delete" {
				deleteToken = answer.token
			}

			env.logAnswer(t, "DELETE manifest by tag with a token for "+actions,
				env.call(t, http.MethodDelete, answer.token, repo, "manifests", scopeTags[actions]))
		}
	})

	if deleteToken == "" {
		t.Fatal("no token for pull,push,delete; the delete probes can't run")
	}

	t.Run("DELETE manifest by tag", func(t *testing.T) {
		digest := digestOf(t, imageA).String()

		env.logAnswer(t, "DELETE manifest by tag", env.call(t, http.MethodDelete, deleteToken, repo, "manifests", "by-tag"))
		env.logAnswer(t, "HEAD the tag afterwards", env.call(t, http.MethodHead, deleteToken, repo, "manifests", "by-tag"))
		env.logAnswer(t, "HEAD its digest afterwards", env.call(t, http.MethodHead, deleteToken, repo, "manifests", digest))
	})

	t.Run("DELETE blob", func(t *testing.T) {
		layers, err := blobImage.Layers()
		require.NoError(t, err)
		layer, err := layers[0].Digest()
		require.NoError(t, err)

		env.logAnswer(t, "DELETE blob", env.call(t, http.MethodDelete, deleteToken, repo, "blobs", layer.String()))
		env.logAnswer(t, "HEAD the blob afterwards", env.call(t, http.MethodHead, deleteToken, repo, "blobs", layer.String()))
	})

	t.Run("DELETE manifest by digest", func(t *testing.T) {
		digest := digestOf(t, imageB).String()

		env.logAnswer(t, "DELETE manifest by digest", env.call(t, http.MethodDelete, deleteToken, repo, "manifests", digest))
		env.logAnswer(t, "HEAD the digest afterwards", env.call(t, http.MethodHead, deleteToken, repo, "manifests", digest))
	})
}
