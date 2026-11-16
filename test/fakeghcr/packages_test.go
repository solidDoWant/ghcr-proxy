package fakeghcr_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/test/fakeghcr"
)

type listedVersion struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Metadata struct {
		PackageType string `json:"package_type"`
		Container   struct {
			Tags []string `json:"tags"`
		} `json:"container"`
	} `json:"metadata"`
}

// apiCall sends method to the fake's API at escapedPath (already escaped) with
// the given Authorization header value, or none when empty.
func apiCall(t *testing.T, fake *fakeghcr.Fake, method, escapedPath, authorization string) *http.Response {
	t.Helper()

	target, err := url.Parse(fake.APIURL() + escapedPath)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), method, target.String(), nil)
	require.NoError(t, err)

	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	resp, err := (&http.Client{Transport: fake.Transport()}).Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	return resp
}

func decodeVersions(t *testing.T, resp *http.Response) []listedVersion {
	t.Helper()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var versions []listedVersion
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&versions))

	return versions
}

func pushRandom(t *testing.T, fake *fakeghcr.Fake, repoAndTag string) string {
	t.Helper()

	image, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push(repoAndTag, image))

	digest, err := image.Digest()
	require.NoError(t, err)

	return digest.String()
}

func TestPackagesAPIListsAndDeletesTwoSegmentPackageVersions(t *testing.T) {
	tests := []struct {
		name      string
		ownerType string
		org       bool
	}{
		{name: "user owner", ownerType: "users"},
		{name: "org owner", ownerType: "orgs", org: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := fakeghcr.New(t, fakeghcr.Options{})
			if test.org {
				fake.SetOrg("Owner")
			}

			digest := pushRandom(t, fake, "owner/a/b:1")
			bearer := "Bearer " + fake.PAT()
			listPath := test.ownerType + "/Owner/packages/container/a%2Fb/versions"

			fake.ResetLog()

			versions := decodeVersions(t, apiCall(t, fake, http.MethodGet, listPath+"?per_page=100", bearer))
			require.Len(t, versions, 1)
			assert.Equal(t, digest, versions[0].Name)
			assert.Equal(t, "container", versions[0].Metadata.PackageType)
			assert.Equal(t, []string{"1"}, versions[0].Metadata.Container.Tags)

			deletePath := listPath + "/" + strconv.FormatInt(versions[0].ID, 10)
			resp := apiCall(t, fake, http.MethodDelete, deletePath, "token "+fake.PAT())
			assert.Equal(t, http.StatusNoContent, resp.StatusCode)

			log := fake.Log()
			require.Len(t, log, 2)
			assert.Equal(t, fakeghcr.Entry{
				Server: "api", Method: http.MethodGet, Path: "/" + test.ownerType + "/Owner/packages/container/a/b/versions",
				EscapedPath: "/" + listPath, QueryKeys: []string{"per_page"}, Credential: fakeghcr.CredPATBearer, Status: http.StatusOK,
			}, log[0])
			assert.Equal(t, "/"+deletePath, log[1].EscapedPath)
			assert.Equal(t, http.StatusNoContent, log[1].Status)

			for _, reference := range []string{"owner/a/b@" + digest, "owner/a/b:1"} {
				parsed, err := name.ParseReference(fake.RegistryHost() + "/" + reference)
				require.NoError(t, err)

				_, err = remote.Head(parsed, fake.RemoteOptions(true)...)
				require.Error(t, err, "%s is still in the registry", reference)
			}

			resp = apiCall(t, fake, http.MethodGet, listPath, bearer)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, "a package with no versions left is gone")
		})
	}
}

func TestPackagesAPIFindsAnOwnerOnlyUnderItsOwnType(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	fake.SetOrg("org")
	pushRandom(t, fake, "org/app:1")
	pushRandom(t, fake, "user/app:1")
	bearer := "Bearer " + fake.PAT()

	assert.Equal(t, http.StatusNotFound, apiCall(t, fake, http.MethodGet, "users/org/packages/container/app/versions", bearer).StatusCode)
	assert.Equal(t, http.StatusNotFound, apiCall(t, fake, http.MethodGet, "orgs/user/packages/container/app/versions", bearer).StatusCode)
	assert.Equal(t, http.StatusNotFound, apiCall(t, fake, http.MethodDelete, "users/org/packages/container/app/versions/1", bearer).StatusCode)
	assert.Equal(t, http.StatusOK, apiCall(t, fake, http.MethodGet, "orgs/org/packages/container/app/versions", bearer).StatusCode)
	assert.Equal(t, http.StatusOK, apiCall(t, fake, http.MethodGet, "users/user/packages/container/app/versions", bearer).StatusCode)
}

func TestPackagesAPIDoesNotSplitAnUnescapedPackageName(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	pushRandom(t, fake, "owner/a/b:1")

	resp := apiCall(t, fake, http.MethodGet, "users/owner/packages/container/a/b/versions", "Bearer "+fake.PAT())
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPackagesAPIRequiresThePAT(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	digest := pushRandom(t, fake, "owner/app:1")
	path := "users/owner/packages/container/app/versions"

	for _, authorization := range []string{"", "Bearer wrong", "Basic " + fake.PAT()} {
		assert.Equal(t, http.StatusUnauthorized, apiCall(t, fake, http.MethodGet, path, authorization).StatusCode, authorization)
		assert.Equal(t, http.StatusUnauthorized, apiCall(t, fake, http.MethodDelete, path+"/1", authorization).StatusCode, authorization)
	}

	versions := decodeVersions(t, apiCall(t, fake, http.MethodGet, path, "Bearer "+fake.PAT()))
	require.Len(t, versions, 1, "an unauthenticated delete removed the version")
	assert.Equal(t, digest, versions[0].Name)
}

func TestPackagesAPIPagesVersionsWithANextLink(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})

	var pushed []string
	for tag := range 3 {
		pushed = append(pushed, pushRandom(t, fake, "owner/app:"+strconv.Itoa(tag)))
	}

	bearer := "Bearer " + fake.PAT()

	first := apiCall(t, fake, http.MethodGet, "users/owner/packages/container/app/versions?per_page=2", bearer)
	firstPage := decodeVersions(t, first)
	require.Len(t, firstPage, 2)

	links := first.Header.Get("Link")
	require.Contains(t, links, `rel="next"`)

	next, err := url.Parse(links[1 : len(links)-len(`>; rel="next"`)])
	require.NoError(t, err)
	assert.Equal(t, "2", next.Query().Get("page"))
	assert.Equal(t, "2", next.Query().Get("per_page"))

	second := apiCall(t, fake, http.MethodGet, next.RequestURI()[1:], bearer)
	secondPage := decodeVersions(t, second)
	require.Len(t, secondPage, 1)
	assert.Empty(t, second.Header.Get("Link"), "the last page has no next link")

	var listed []string
	for _, version := range append(firstPage, secondPage...) {
		listed = append(listed, version.Name)
	}

	assert.ElementsMatch(t, pushed, listed)
}
