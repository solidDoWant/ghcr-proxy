//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func connect(t *testing.T, namespace string) error {
	t.Helper()

	_, err := cluster.kubectl(t.Context(), "", "-n", namespace, "run", "np-probe", "--rm", "-i", "--restart=Never",
		"--image="+dockerCLIImage, "--image-pull-policy=IfNotPresent", "--command", "--",
		"nc", "-z", "-w", "5", proxyClusterIP, "443")

	return err
}

func TestNetworkPolicyBlocksNamespacesOutsideTheClientSelector(t *testing.T) {
	for _, tenant := range cluster.tenants {
		require.NoError(t, connect(t, tenant.namespace), "a client namespace must reach the proxy")
	}

	assert.Error(t, connect(t, cluster.upstreams[0].namespace), "a namespace outside the selector must not reach the proxy")
}
