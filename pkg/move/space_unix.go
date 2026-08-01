//go:build unix

package move

import "syscall"

// availableBytes reports free space on the filesystem holding dir, for the
// unprivileged user — Bavail rather than Bfree, since the reserved blocks are
// not ours to fill. A directory that does not exist yet answers 0, which the
// preflight treats as "unknown" rather than "full": refusing a move because a
// scratch directory has not been created is a worse failure than checking the
// space after mkdir.
func availableBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
