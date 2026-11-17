package server_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/secrets"
)

func TestInterimResponseHeadersDoNotReachClient(t *testing.T) {
	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}

		w.Header().Set("WWW-Authenticate", `Bearer realm="https://elsewhere.example/token"`)
		w.Header().Set("Location", "https://elsewhere.example/x")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("WWW-Authenticate")
		w.Header().Del("Location")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstreamServer.Close)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstreamServer.Certificate().Raw})
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		objects.upstream.Spec.URL = upstreamServer.URL
		objects.secrets[secrets.Key{Namespace: adminNamespace, Name: "upstream-a-ca"}] = secrets.Secret{
			State: secrets.Present, ResourceVersion: "1", Data: secrets.Data{"ca.crt": caPEM},
		}
	}})

	var interim []textproto.MIMEHeader

	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(_ int, header textproto.MIMEHeader) error {
			interim = append(interim, header)
			return nil
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.url()+"/v2/soliddowant/foo/source/manifests/latest", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", basic(builderToken))

	resp, err := proxy.client().Do(req)
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
