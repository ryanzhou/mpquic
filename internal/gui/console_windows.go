package gui

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
)

func consoleWindow() uintptr {
	h, _, _ := procGetConsoleWindow.Call()
	return h
}

// OwnsConsole reports whether the process was started without a terminal,
// i.e. double-clicked in Explorer: either it has no console at all (the
// manifest's consoleAllocationPolicy "detached", Windows 11 24H2+), or
// Windows created a console just for it (older versions).
func OwnsConsole() bool {
	if consoleWindow() == 0 {
		h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
		return err != nil || h == 0 || h == windows.InvalidHandle
	}
	var pids [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

// hideOwnConsole closes the console window Windows opened for us (older
// versions without consoleAllocationPolicy support), leaving a terminal we
// were started from alone.
func hideOwnConsole() {
	if consoleWindow() != 0 && OwnsConsole() {
		procFreeConsole.Call()
	}
}

// relaunchElevated starts this executable again with args through the UAC
// prompt. It returns windows.ERROR_CANCELLED if the user declined.
func relaunchElevated(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = windows.EscapeArg(a)
	}
	cwd, _ := os.Getwd()
	return windows.ShellExecute(0, windows.StringToUTF16Ptr("runas"), windows.StringToUTF16Ptr(exe),
		windows.StringToUTF16Ptr(strings.Join(quoted, " ")), windows.StringToUTF16Ptr(cwd), windows.SW_SHOWNORMAL)
}

func messageBox(title, text string, flags uint32) {
	windows.MessageBox(0, windows.StringToUTF16Ptr(text), windows.StringToUTF16Ptr(title), flags)
}
