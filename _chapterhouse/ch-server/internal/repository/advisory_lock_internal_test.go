package repository

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDisposeAdvisoryLockConn_ErrorDestroysSuccessReleases pins the
// destroy-vs-release branch selection at the heart of the advisory-lock
// hardening: an unlock error must never result in the connection going
// back to the pool, because a "healthy"-looking pooled connection that
// secretly still holds the workspace's advisory lock would 409 every
// future consolidation run against that workspace until the pool happened
// to recycle it away. This is a pure decision function (two callables, no
// live connection) so the branch selection is verified without a DB.
func TestDisposeAdvisoryLockConn_ErrorDestroysSuccessReleases(t *testing.T) {
	t.Run("unlock error destroys, never releases", func(t *testing.T) {
		var destroyed, released int
		disposeAdvisoryLockConn(errors.New("unlock boom"),
			func() { destroyed++ },
			func() { released++ },
		)
		require.Equal(t, 1, destroyed)
		require.Equal(t, 0, released)
	})

	t.Run("unlock success releases, never destroys", func(t *testing.T) {
		var destroyed, released int
		disposeAdvisoryLockConn(nil,
			func() { destroyed++ },
			func() { released++ },
		)
		require.Equal(t, 0, destroyed)
		require.Equal(t, 1, released)
	})
}
