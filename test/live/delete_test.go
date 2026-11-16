//go:build live

package live

import (
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

func TestDeleteByDigestThroughTheProxy(t *testing.T) {
	env := requireLive(t)
	repo := env.repo("delete/by-digest")

	// GitHub refuses to delete a package's last version, so the package
	// keeps a second one.
	env.pushThroughProxy(t, repo, "keep")
	doomed := env.pushThroughProxy(t, repo, "doomed")
	ref := env.proxyRef(t, repo+"@"+digestOf(t, doomed).String())

	require.NoError(t, remote.Delete(ref, env.callerOptions(nil)...))

	require.Eventually(t, func() bool {
		_, err := remote.Head(ref, env.callerOptions(nil)...)
		return err != nil
	}, time.Minute, 2*time.Second, "the deleted manifest is still served")
}
