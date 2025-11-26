//go:build !windows

package platform

import (
	"os"
	"os/signal"
	"syscall"
)

// NotifySignals registers for interrupt and termination signals on Unix-like OSes.
func NotifySignals(ch chan os.Signal) {
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
}
