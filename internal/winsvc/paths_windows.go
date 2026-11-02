package winsvc

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// DataDir is %ProgramData%\MPQUIC (configs and logs).
func DataDir() string {
	p, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil || p == "" {
		if p = os.Getenv("ProgramData"); p == "" {
			p = `C:\ProgramData`
		}
	}
	return filepath.Join(p, "MPQUIC")
}

// InstallDir is %ProgramFiles%\MPQUIC (the service binary).
func InstallDir() string {
	p, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil || p == "" {
		if p = os.Getenv("ProgramFiles"); p == "" {
			p = `C:\Program Files`
		}
	}
	return filepath.Join(p, "MPQUIC")
}

// InstalledExe is the path the service runs from.
func InstalledExe() string { return filepath.Join(InstallDir(), "mpquic.exe") }

// ConfigPath is the config file of a service instance.
func ConfigPath(name string) string { return filepath.Join(DataDir(), name+".json") }

// LogPath is the log file of a service instance.
func LogPath(name string) string { return filepath.Join(DataDir(), "logs", name+".log") }

// Directory ACLs: SYSTEM and Administrators have full control; the service
// account (LOCAL SERVICE) may read configs (they contain the password, so
// ordinary users get no access) and modify logs. Inheritance is disabled.
const (
	dataDirSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FR;;;LS)"
	logsDirSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;LS)"
)

// EnsureDirs creates the data and log directories with restrictive ACLs.
func EnsureDirs() error {
	if err := ensureDir(DataDir(), dataDirSDDL); err != nil {
		return err
	}
	return ensureDir(filepath.Join(DataDir(), "logs"), logsDirSDDL)
}

func ensureDir(dir, sddl string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	// SetNamedSecurityInfo propagates the inheritable ACEs to existing files.
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// IsElevated reports whether the process runs with an elevated token.
func IsElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }
