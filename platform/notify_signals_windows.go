//go:build windows

package platform

import (
	"os"
	"os/signal"
)

// NotifySignals registers only interrupt on Windows.
func NotifySignals(ch chan os.Signal) {
	signal.Notify(ch, os.Interrupt)
}
