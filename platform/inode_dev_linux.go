//go:build linux

package platform

import (
	"os"
	"syscall"
)

// InodeDev returns inode and device id on Linux using POSIX stat.
func InodeDev(fi os.FileInfo) (uint64, uint64) {
	if fi == nil {
		return 0, 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino, st.Dev
	}
	return 0, 0
}
