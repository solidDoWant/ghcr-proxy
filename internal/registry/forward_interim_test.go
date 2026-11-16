package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/upstream"
)

type interimTokens struct{}

func (interimTokens) Token(_ context.Context, _ upstream.Kind, _ []string) (upstream.Token, error) {
	return upstream.Token{Value: "upstream-token"}, nil
}
func (interimTokens) Invalidate(upstream.Token) {}

func TestInterimResponseHeadersDoNotReachClient(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://elsewhere.example/token"`)
		w.Header().Set("Location", "https://elsewhere.example/x")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("WWW-Authenticate")
		w.Header().Del("Location")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstreamServer.Close()

	upstreamURL, err := url.Parse(upstreamServer.URL)
	require.NoError(t, err)

	forwarder := NewForwarder(ForwarderConfig{Upstream: upstreamURL, Tokens: interimTokens{}})

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarder.ServeForward(w, r, Forward{Request: Request{
			Method: http.MethodGet, Route: RouteManifest, Action: v1alpha1.ActionPull, Name: "o/r", Reference: "latest",
		}, Credential: CredentialPAT})
	}))
	defer proxy.Close()

	var interim []textproto.MIMEHeader

	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(_ int, header textproto.MIMEHeader) error {
			interim = append(interim, header)
			return nil
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/v2/o/r/manifests/latest", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Www-Authenticate"))
	assert.Empty(t, resp.Header.Get("Location"))

	for _, header := range interim {
		assert.Empty(t, header.Get("Www-Authenticate"))
		assert.Empty(t, header.Get("Location"))
	}
}
