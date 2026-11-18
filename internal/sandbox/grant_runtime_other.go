//go:build !windows

package sandbox

import (
	"errors"
	"fmt"
)

// GrantAgentRuntimeRead is the Windows AppContainer's pre-grant of a
// kernel-known read-only tree (ADR-0157 D7.7.5). The Unix runners show a
// read-only bind by path (bubblewrap, docker, Seatbelt), so there is nothing to
// pre-grant and the tree is recorded as not granted.
func GrantAgentRuntimeRead(dir string) error {
	return fmt.Errorf("pre-grant of %s: %w (the agent runners on this OS bind paths, they need no ACE)", dir, errors.ErrUnsupported)
}
