//go:build unix

package rowpack

import (
	"runtime"
	"syscall"
)

// peakRSSBytes returns the process peak resident set size. ru_maxrss units
// differ by platform: Linux reports kilobytes, macOS/BSD report bytes.
func peakRSSBytes() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "linux" {
		return ru.Maxrss * 1024
	}
	return ru.Maxrss
}
