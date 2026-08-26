package update

import (
	"fmt"
	"os"
	"runtime"
)

// replaceBinary puts staged in place of target.
//
// On POSIX this is one rename(2): the directory entry flips from the old inode
// to the new one in a single step, and a process already running the old binary
// keeps running it from the unlinked inode until it exits. There is no window in
// which the path is missing, truncated, or half-written — which matters, because
// the path being replaced is the one a service manager will exec on restart.
//
// Windows will not rename over a file that is mapped for execution, so the
// running binary is first moved aside to <target>.old and the new one takes its
// place. The old file is deleted on a best effort: while the process is running
// it is still locked, so it is left behind and swept on the next update. A
// leftover .old next to the binary is untidy, not broken, and is a better
// failure than an update that cannot be applied at all.
func replaceBinary(target, staged string) error {
	if runtime.GOOS != "windows" {
		if err := os.Rename(staged, target); err != nil {
			return fmt.Errorf("update: install over %s: %w", target, err)
		}
		return nil
	}

	old := target + ".old"
	_ = os.Remove(old) // sweep a previous update's leftover before reusing the name
	if err := os.Rename(target, old); err != nil {
		return fmt.Errorf("update: move the running binary aside: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		// Put it back. Failing an update is recoverable; leaving the machine with
		// no pcsync at all is not.
		if restoreErr := os.Rename(old, target); restoreErr != nil {
			return fmt.Errorf("update: install over %s failed (%w) and the original could not be restored from %s: %w",
				target, err, old, restoreErr)
		}
		return fmt.Errorf("update: install over %s: %w", target, err)
	}
	_ = os.Remove(old)
	return nil
}
