package uploadtoken_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/uploadtoken"
)

const (
	upstreamUID = "11111111-2222-3333-4444-555555555555"
	name        = "owner/image"
	digestA     = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var (
	key      = []byte("0123456789abcdef0123456789abcdef")
	upstream = &url.URL{Scheme: "https", Host: "registry.example"}
)

func newCodec(t *testing.T) *uploadtoken.Codec {
	t.Helper()

	codec, err := uploadtoken.New(key)
	require.NoError(t, err)

	return codec
}

// forge builds a token for suffix the way the design specifies, independently of the codec, so the structural
// checks can be reached with a valid signature.
func forge(signingKey []byte, uid, repository, suffix string) string {
	mac := hmac.New(sha256.New, signingKey)
	mac.Write([]byte(uid + "\n" + repository + "\n" + suffix))

	return base64.RawURLEncoding.EncodeToString([]byte(suffix)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func tokenOf(t *testing.T, location string) string {
	t.Helper()

	token, found := strings.CutPrefix(location, "/v2/"+name+"/blobs/uploads/")
	require.True(t, found, "client location %q is not an upload session path", location)

	return token
}

func TestKeyShorterThan32BytesIsRefused(t *testing.T) {
	_, err := uploadtoken.New(key[:31])
	require.Error(t, err)

	_, err = uploadtoken.New(key[:32])
	require.NoError(t, err)
}

func TestIssuedTokensRoundTripToTheUpstreamLocation(t *testing.T) {
	codec := newCodec(t)

	cases := []struct {
		name     string
		location string
		wantURL  string
	}{
		{"singular upload session", "/v2/owner/image/blobs/upload/session-1", "/v2/owner/image/blobs/upload/session-1"},
		{
			"plural upload session with state", "/v2/owner/image/blobs/uploads/session-1?state=x",
			"/v2/owner/image/blobs/uploads/session-1?state=x",
		},
		{
			"absolute location on the upstream host", "https://registry.example/v2/owner/image/blobs/uploads/abc?_state=a%3D%3D",
			"/v2/owner/image/blobs/uploads/abc?_state=a%3D%3D",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			location, err := codec.Issue(upstreamUID, name, upstream, testCase.location)
			require.NoError(t, err)

			token := tokenOf(t, location)
			assert.True(t, uploadtoken.Valid(token))
			assert.NotContains(t, token, "session", "the token is opaque")

			upstreamURL, err := codec.Open(upstreamUID, name, token)
			require.NoError(t, err)
			assert.Equal(t, testCase.wantURL, upstreamURL.String())
		})
	}

	t.Run("relative location resolves against the upstream request URL", func(t *testing.T) {
		base := &url.URL{Scheme: "https", Host: "registry.example", Path: "/v2/owner/image/blobs/uploads/"}

		location, err := codec.Issue(upstreamUID, name, base, "session-3")
		require.NoError(t, err)

		upstreamURL, err := codec.Open(upstreamUID, name, tokenOf(t, location))
		require.NoError(t, err)
		assert.Equal(t, "/v2/owner/image/blobs/uploads/session-3", upstreamURL.String())
	})
}

func TestUnusableUpstreamLocationsAreRefused(t *testing.T) {
	codec := newCodec(t)

	locations := []struct {
		name     string
		location string
	}{
		{"another host", "https://other.example/v2/owner/image/blobs/uploads/s"},
		{"another port", "https://registry.example:8443/v2/owner/image/blobs/uploads/s"},
		{"another scheme", "http://registry.example/v2/owner/image/blobs/uploads/s"},
		{"user info", "https://user@registry.example/v2/owner/image/blobs/uploads/s"},
		{"another repository", "/v2/owner/other/blobs/uploads/s"},
		{"a repository with the name as prefix", "/v2/owner/image2/blobs/uploads/s"},
		{"the repository's manifests", "/v2/owner/image/manifests/latest"},
		{"outside v2", "/other/owner/image/blobs/uploads/s"},
		{"the blobs path itself", "/v2/owner/image/blobs/"},
		{"empty segment", "/v2/owner/image/blobs/uploads//s"},
		{"dot-dot segment", "/v2/owner/image/blobs/uploads/s/../../../other/blobs/x"},
		{"encoded dot-dot climbing out", "/v2/owner/image/blobs/%2E%2E/%2E%2E/other/blobs/x"},
		{"payload over the size limit", "/v2/owner/image/blobs/uploads/" + strings.Repeat("s", 2100)},
	}
	for _, testCase := range locations {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := codec.Issue(upstreamUID, name, upstream, testCase.location)
			require.ErrorIs(t, err, uploadtoken.ErrUnusableLocation)
		})
	}

	t.Run("payload at the size limit", func(t *testing.T) {
		// 2100 suffix bytes encode to exactly 2800 base64url characters.
		suffix := "uploads/" + strings.Repeat("s", 2100-len("uploads/"))

		location, err := codec.Issue(upstreamUID, name, upstream, "/v2/owner/image/blobs/"+suffix)
		require.NoError(t, err)

		payload, _, _ := strings.Cut(tokenOf(t, location), ".")
		assert.Len(t, payload, 2800)
	})
}

func TestTokensThatWerentIssuedForTheRequestAreUnknown(t *testing.T) {
	codec := newCodec(t)

	location, err := codec.Issue(upstreamUID, name, upstream, "/v2/owner/image/blobs/uploads/session-1")
	require.NoError(t, err)

	token := tokenOf(t, location)
	payload, mac, _ := strings.Cut(token, ".")

	otherKey := append([]byte(nil), key...)
	otherKey[0] ^= 1

	longSuffix := strings.Repeat("s", 2101)

	cases := []struct {
		name  string
		uid   string
		repo  string
		token string
	}{
		{"another repository", upstreamUID, "owner/other", token},
		{"another upstream", "99999999-2222-3333-4444-555555555555", name, token},
		{"another key", upstreamUID, name, forge(otherKey, upstreamUID, name, "uploads/session-1")},
		{"changed payload", upstreamUID, name, forge(key, upstreamUID, name, "uploads/session-2")[:len(payload)] + "." + mac},
		{"no signature", upstreamUID, name, payload},
		{"short signature", upstreamUID, name, payload + "." + mac[:42]},
		{"padded payload", upstreamUID, name, payload + "=." + mac},
		{"invalid base64", upstreamUID, name, "a." + mac},
		{"non-canonical base64", upstreamUID, name, payload[:len(payload)-1] + "B." + mac},
		{"empty payload", upstreamUID, name, "." + mac},
		{"payload over the size limit", upstreamUID, name, forge(key, upstreamUID, name, longSuffix)},
		{"signed path outside the blobs", upstreamUID, name, forge(key, upstreamUID, name, "x/../../manifests/latest")},
		{"signed dot segment", upstreamUID, name, forge(key, upstreamUID, name, "uploads/./s")},
		{"signed empty segment", upstreamUID, name, forge(key, upstreamUID, name, "uploads//s")},
		{"signed fragment", upstreamUID, name, forge(key, upstreamUID, name, "uploads/s#x")},
		{"signed fragment in the query", upstreamUID, name, forge(key, upstreamUID, name, "uploads/s?state=x#y")},
		{"signed empty suffix", upstreamUID, name, forge(key, upstreamUID, name, "")},
		{"signed encoded dot-dot", upstreamUID, name, forge(key, upstreamUID, name, "%2E%2E/%2E%2E/manifests/latest")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := codec.Open(testCase.uid, testCase.repo, testCase.token)
			require.ErrorIs(t, err, uploadtoken.ErrUnknown)
		})
	}

	t.Run("a correctly signed suffix opens", func(t *testing.T) {
		upstreamURL, err := codec.Open(upstreamUID, name, forge(key, upstreamUID, name, "uploads/s?state=x"))
		require.NoError(t, err)
		assert.Equal(t, "/v2/owner/image/blobs/uploads/s?state=x", upstreamURL.String())
	})
}

func TestUploadSessionClientQueryAllowsOnlyANewDigestOnPut(t *testing.T) {
	cases := []struct {
		name      string
		upstream  string
		method    string
		query     string
		wantURL   string
		assertErr require.ErrorAssertionFunc
	}{
		{name: "no client query", upstream: "/v2/owner/image/blobs/uploads/s?state=x", method: "PATCH",
			wantURL: "/v2/owner/image/blobs/uploads/s?state=x"},
		{name: "digest on put", upstream: "/v2/owner/image/blobs/uploads/s?state=x", method: "PUT",
			query: "digest=" + digestA, wantURL: "/v2/owner/image/blobs/uploads/s?state=x&digest=sha256%3A" + digestA[7:]},
		{name: "digest on put with no upstream query", upstream: "/v2/owner/image/blobs/upload/s", method: "PUT",
			query: "digest=" + digestA, wantURL: "/v2/owner/image/blobs/upload/s?digest=sha256%3A" + digestA[7:]},
		{name: "digest also in the decoded query", upstream: "/v2/owner/image/blobs/uploads/s?digest=" + digestA,
			method: "PUT", query: "digest=" + digestA},
		{name: "another key", upstream: "/v2/owner/image/blobs/uploads/s?state=x", method: "PUT",
			query: "digest=" + digestA + "&state=y"},
		{name: "a key from the decoded query", upstream: "/v2/owner/image/blobs/uploads/s?state=x", method: "PATCH",
			query: "state=y"},
		{name: "digest on patch", upstream: "/v2/owner/image/blobs/uploads/s", method: "PATCH", query: "digest=" + digestA},
		{name: "two digests", upstream: "/v2/owner/image/blobs/uploads/s", method: "PUT",
			query: "digest=" + digestA + "&digest=" + digestA},
		{name: "malformed query", upstream: "/v2/owner/image/blobs/uploads/s", method: "PUT", query: "digest=%zz"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assertErr := testCase.assertErr
			if assertErr == nil {
				assertErr = require.NoError
				if testCase.wantURL == "" {
					assertErr = func(t require.TestingT, err error, _ ...any) {
						require.ErrorIs(t, err, uploadtoken.ErrQuery)
					}
				}
			}

			upstreamURL, err := url.Parse(testCase.upstream)
			require.NoError(t, err)

			err = uploadtoken.AddClientQuery(upstreamURL, testCase.method, testCase.query)
			assertErr(t, err)

			if testCase.wantURL != "" {
				assert.Equal(t, testCase.wantURL, upstreamURL.String())
			}
		})
	}
}
