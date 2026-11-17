package mirror_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/registrygate/internal/mirror"
)

// A zero token timeout is accepted by the CRD; the upstream client treats it as the default.
func TestZeroTokenTimeoutUsesTheDefault(t *testing.T) {
	fake := newFakeMirror(t)
	object := fake.model(t)
	object.Object.Spec.Timeouts.Token = &metav1.Duration{}

	client, err := mirror.NewClientFromMirror(object, mirror.ClientOptions{})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, client.RequestURL("/v2/a/manifests/b", "").String(), nil)
	require.NoError(t, err)

	resp, err := client.Send(req, "repository:a:pull")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
