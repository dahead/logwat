//go:build windows

package platform

import "os"

// InodeDev on Windows returns zeros as a minimal, path-only tracking fallback.
// For full fidelity across rotations, consider implementing with x/sys/windows
// using GetFileInformationByHandle to return (FileIndex, VolumeSerialNumber).
func InodeDev(fi os.FileInfo) (uint64, uint64) {
	return 0, 0
}
