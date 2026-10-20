//go:build !linux && !darwin

package main

import (
	"fmt"
	"io"
	"time"
)

func reportStats(w io.Writer, cmd string, elapsed time.Duration) {
	_, _ = fmt.Fprintf(w, "stats: cmd=%s wall=%s\n", cmd, elapsed.Round(time.Millisecond))
}
