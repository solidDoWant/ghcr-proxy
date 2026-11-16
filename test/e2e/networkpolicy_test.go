//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dockerCLIImage is the image of the CI pod's docker container; it carries a busybox nc.
const dockerCLIImage = "mirror.gcr.io/library/docker:29.9.3-cli@sha256:5a7af0dcb0e46c803efe9b45371dd21cb500e5b4453250394ac6f06d49b6e6f1"

func connect(t *testing.T, namespace string) error {
	t.Helper()

	_, err := cluster.kubectl(t.Context(), "", "-n", namespace, "run", "np-probe", "--rm", "-i", "--restart=Never",
		"--image="+dockerCLIImage, "--image-pull-policy=IfNotPresent", "--command", "--",
		"nc", "-z", "-w", "5", proxyClusterIP, "443")

	return err
}

func TestNetworkPolicyBlocksNamespacesOutsideTheClientSelector(t *testing.T) {
	require.NoError(t, connect(t, ciNamespace), "a client namespace must reach the proxy")
	assert.Error(t, connect(t, fakeNamespace), "a namespace outside the selector must not reach the proxy")
}
