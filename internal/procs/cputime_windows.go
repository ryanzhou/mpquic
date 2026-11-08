package procs

import (
	"time"

	"golang.org/x/sys/windows"
)

// processCPU returns the CPU time (user + kernel) used by the process.
func processCPU() (time.Duration, bool) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	ft := func(f windows.Filetime) time.Duration {
		return time.Duration(uint64(f.HighDateTime)<<32|uint64(f.LowDateTime)) * 100
	}
	return ft(kernel) + ft(user), true
}
