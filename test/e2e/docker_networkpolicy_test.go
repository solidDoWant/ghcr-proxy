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

// This file name sorts before docs_test.go on purpose: the docs walk-through
// replaces the proxy release with one that has no NetworkPolicy, and Go runs
// tests in file-name order.
func TestNetworkPolicyBlocksNamespacesOutsideTheClientSelector(t *testing.T) {
	require.NoError(t, connect(t, ciNamespace), "a client namespace must reach the proxy")
	assert.Error(t, connect(t, fakeNamespace), "a namespace outside the selector must not reach the proxy")
}
