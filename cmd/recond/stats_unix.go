//go:build linux || darwin

package main

import (
	"fmt"
	"io"
	"runtime"
	"syscall"
	"time"
)

// reportStats prints wall time and peak resident memory (RECOND_STATS=1),
// which is what the README benchmarks quote.
func reportStats(w io.Writer, cmd string, elapsed time.Duration) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return
	}
	maxrss := ru.Maxrss * 1024 // kilobytes on Linux
	if runtime.GOOS == "darwin" {
		maxrss = ru.Maxrss // bytes on macOS
	}
	cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	_, _ = fmt.Fprintf(w, "stats: cmd=%s wall=%s cpu=%s peak_rss=%.1fMiB\n", cmd, elapsed.Round(time.Millisecond), cpu.Round(time.Millisecond), float64(maxrss)/(1<<20))
}
