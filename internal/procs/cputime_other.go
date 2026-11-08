//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package procs

import "time"

// processCPU is not available: the governor stays off.
func processCPU() (time.Duration, bool) { return 0, false }
