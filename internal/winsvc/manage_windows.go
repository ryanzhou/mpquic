package winsvc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"mpquic/internal/app"
	"mpquic/internal/config"
)

// ServiceAccount is the account the service runs as.
const ServiceAccount = `NT AUTHORITY\LocalService`

// ErrNotElevated is returned by operations that need administrator rights.
var ErrNotElevated = errors.New("administrator rights are required (run from an elevated prompt)")

// ErrNotInstalled is returned for a service that does not exist.
var ErrNotInstalled = errors.New("service is not installed")

// InstallOptions configure Install.
type InstallOptions struct {
	Name   string
	Config *config.Config // must have Role set
	// Start starts the service after installing. A service that was running
	// before the (re)install is restarted regardless.
	Start bool
	// Progress, if set, receives human-readable progress messages.
	Progress func(string)
}

// Install writes the config, copies the binary to InstalledExe, creates or
// updates the service, its recovery policy, event log source and firewall
// rules, and (re)starts it as requested.
func Install(o InstallOptions) error {
	say := o.Progress
	if say == nil {
		say = func(string) {}
	}
	if err := CheckName(o.Name); err != nil {
		return err
	}
	cfg := o.Config.Clone()
	role, ok := app.NormalizeRole(cfg.Role)
	if !ok {
		return fmt.Errorf("the config has no valid \"role\" (local, tunnel, remote or relay): %q", cfg.Role)
	}
	cfg.Role = role
	for _, p := range []*string{&cfg.Cert, &cfg.Key} {
		if *p != "" {
			abs, err := filepath.Abs(*p)
			if err != nil {
				return err
			}
			*p = abs
		}
	}
	if err := app.Validate(role, cfg); err != nil {
		return err
	}
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(o.Name)
	exists := err == nil
	wasRunning := false
	if exists {
		defer s.Close()
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State != svc.Stopped {
			wasRunning = true
			say("Stopping " + o.Name)
			if err := stopAndWait(s, 30*time.Second); err != nil {
				return err
			}
		}
	}

	if err := EnsureDirs(); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return ErrNotElevated
		}
		return fmt.Errorf("data directory %s: %w", DataDir(), err)
	}
	if err := cfg.Save(ConfigPath(o.Name)); err != nil {
		return err
	}
	say("Wrote " + ConfigPath(o.Name))

	exe := InstalledExe()
	src, err := os.Executable()
	if err != nil {
		return err
	}
	if needsCopy(src, exe) {
		// Other instances run from the same binary: stop them while it is
		// replaced, and restart those that were running.
		others, _ := instancesUsing(m, exe)
		var restart []string
		for _, n := range others {
			if strings.EqualFold(n, o.Name) {
				continue
			}
			if other, err := m.OpenService(n); err == nil {
				if st, err := other.Query(); err == nil && st.State != svc.Stopped {
					say("Stopping " + n + " to update the binary")
					if err := stopAndWait(other, 30*time.Second); err == nil {
						restart = append(restart, n)
					}
				}
				other.Close()
			}
		}
		err := copyFile(src, exe)
		for _, n := range restart {
			if other, err := m.OpenService(n); err == nil {
				if err := startAndWait(other, n); err != nil {
					say("Warning: " + err.Error())
				}
				other.Close()
			}
		}
		if err != nil {
			return fmt.Errorf("installing %s: %w", exe, err)
		}
		say("Installed " + exe)
	}

	sc := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		ServiceStartName: ServiceAccount,
		DisplayName:      DisplayName(o.Name),
		Description:      "MPQUIC multipath QUIC proxy (" + role + "). Config: " + ConfigPath(o.Name),
	}
	args := []string{"service", "run", "--name", o.Name}
	if exists {
		cur, err := s.Config()
		if err != nil {
			return err
		}
		cur.BinaryPathName = commandLine(exe, args...)
		cur.StartType, cur.ErrorControl = sc.StartType, sc.ErrorControl
		cur.ServiceStartName, cur.Password = sc.ServiceStartName, ""
		cur.DisplayName, cur.Description = sc.DisplayName, sc.Description
		if err := s.UpdateConfig(cur); err != nil {
			return fmt.Errorf("updating service: %w", err)
		}
		say("Updated service " + o.Name)
	} else {
		s, err = m.CreateService(o.Name, exe, sc, args...)
		if err != nil {
			return fmt.Errorf("creating service: %w", err)
		}
		defer s.Close()
		say("Created service " + o.Name)
	}
	// Restart after failures (5 s, 10 s, then every 30 s; counter resets daily).
	if err := setRecovery(s); err != nil {
		say("Warning: recovery actions: " + err.Error())
	}
	if err := eventlog.InstallAsEventCreate(o.Name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil &&
		!strings.Contains(err.Error(), "exists") {
		say("Warning: event log source: " + err.Error())
	}
	rules := FirewallRules(cfg)
	if err := ApplyFirewall(o.Name, exe, rules); err != nil {
		say("Warning: " + err.Error())
	} else {
		for _, r := range rules {
			say("Firewall: allowed inbound " + r.String())
		}
	}
	if o.Start || wasRunning {
		say("Starting " + o.Name)
		return startAndWait(s, o.Name)
	}
	return nil
}

func setRecovery(s *mgr.Service) error {
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, 86400); err != nil {
		return err
	}
	return s.SetRecoveryActionsOnNonCrashFailures(true)
}

func commandLine(exe string, args ...string) string {
	s := windows.EscapeArg(exe)
	for _, a := range args {
		s += " " + windows.EscapeArg(a)
	}
	return s
}

// needsCopy reports whether dst is missing or differs from src (and is not
// the same file).
func needsCopy(src, dst string) bool {
	if sameFile(src, dst) {
		return false
	}
	a, err := os.ReadFile(src)
	if err != nil {
		return true
	}
	b, err := os.ReadFile(dst)
	return err != nil || !bytes.Equal(a, b)
}

// copyFile replaces dst with a copy of src.
func copyFile(src, dst string) error {
	a, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, a, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// Uninstall stops and deletes the service and removes its firewall rules
// and event log source. With purge it also deletes its config and logs, and
// the installed binary if no other instance uses it.
func Uninstall(name string, purge bool, progress func(string)) error {
	say := progress
	if say == nil {
		say = func(string) {}
	}
	if err := CheckName(name); err != nil {
		return err
	}
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err == nil {
		if st, err := s.Query(); err == nil && st.State != svc.Stopped {
			say("Stopping " + name)
			if err := stopAndWait(s, 30*time.Second); err != nil {
				s.Close()
				return err
			}
		}
		err = s.Delete()
		s.Close()
		if err != nil {
			return fmt.Errorf("deleting service: %w", err)
		}
		say("Deleted service " + name)
	} else if !purge {
		return ErrNotInstalled
	}
	RemoveFirewall(name)
	eventlog.Remove(name)
	if purge {
		for _, p := range []string{ConfigPath(name), LogPath(name), LogPath(name) + ".1"} {
			if err := os.Remove(p); err == nil {
				say("Deleted " + p)
			}
		}
		if others, _ := instancesUsing(m, InstalledExe()); len(others) == 0 {
			if err := os.Remove(InstalledExe()); err == nil {
				say("Deleted " + InstalledExe())
				os.Remove(InstallDir())
			}
		}
	}
	return nil
}

// instancesUsing lists the services whose binary is exe.
func instancesUsing(m *mgr.Mgr, exe string) ([]string, error) {
	names, err := m.ListServices()
	if err != nil {
		return nil, err
	}
	var out []string
	want := strings.ToLower(windows.EscapeArg(exe))
	for _, n := range names {
		h, err := windows.OpenService(m.Handle, windows.StringToUTF16Ptr(n), windows.SERVICE_QUERY_CONFIG)
		if err != nil {
			continue
		}
		s := &mgr.Service{Name: n, Handle: h}
		c, err := s.Config()
		s.Close()
		if err == nil && strings.HasPrefix(strings.ToLower(c.BinaryPathName), want+" ") {
			out = append(out, n)
		}
	}
	return out, nil
}

// Instances lists installed services running mpquic and instances that only
// have a config file, sorted, with DefaultName first.
func Instances() []string {
	seen := map[string]bool{}
	out := []string{DefaultName}
	seen[strings.ToLower(DefaultName)] = true
	add := func(n string) {
		if !seen[strings.ToLower(n)] && CheckName(n) == nil {
			seen[strings.ToLower(n)] = true
			out = append(out, n)
		}
	}
	if m, err := mgr.Connect(); err == nil {
		names, _ := instancesUsing(m, InstalledExe())
		m.Disconnect()
		for _, n := range names {
			add(n)
		}
	}
	files, _ := filepath.Glob(filepath.Join(DataDir(), "*.json"))
	for _, f := range files {
		add(strings.TrimSuffix(filepath.Base(f), ".json"))
	}
	return out
}

// Status describes an instance.
type Status struct {
	Installed bool
	State     svc.State
	PID       uint32
	ExitCode  uint32 // service-specific exit code of the last stop
}

func (s Status) String() string {
	if !s.Installed {
		return "not installed"
	}
	switch s.State {
	case svc.Stopped:
		if s.ExitCode != 0 {
			return fmt.Sprintf("stopped (failed, exit code %d)", s.ExitCode)
		}
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.ContinuePending, svc.PausePending, svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("state %d", s.State)
}

// Query returns the status of a service instance. It works without
// administrator rights.
func Query(name string) (Status, error) {
	m, err := connectReadOnly()
	if err != nil {
		return Status{}, err
	}
	defer m.Disconnect()
	h, err := windows.OpenService(m.Handle, windows.StringToUTF16Ptr(name), windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return Status{}, nil
		}
		return Status{}, err
	}
	s := &mgr.Service{Name: name, Handle: h}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return Status{}, err
	}
	return Status{Installed: true, State: st.State, PID: st.ProcessId, ExitCode: st.ServiceSpecificExitCode}, nil
}

func connectReadOnly() (*mgr.Mgr, error) {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, err
	}
	return &mgr.Mgr{Handle: h}, nil
}

// connect opens the service manager with full access, which requires
// administrator rights.
func connect() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, ErrNotElevated
	}
	return m, err
}

func openService(name string) (*mgr.Mgr, *mgr.Service, error) {
	m, err := connect()
	if err != nil {
		return nil, nil, err
	}
	s, err := m.OpenService(name)
	if err != nil {
		m.Disconnect()
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, nil, ErrNotInstalled
		}
		return nil, nil, err
	}
	return m, s, nil
}

// Start starts the service and waits until it runs.
func Start(name string) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return startAndWait(s, name)
}

// Stop stops the service and waits until it has stopped.
func Stop(name string) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return stopAndWait(s, 30*time.Second)
}

// Restart stops (if running) and starts the service.
func Restart(name string) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if err := stopAndWait(s, 30*time.Second); err != nil {
		return err
	}
	return startAndWait(s, name)
}

func stopAndWait(s *mgr.Service, timeout time.Duration) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if st, err = s.Control(svc.Stop); err != nil {
			if !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
				return fmt.Errorf("stopping %s: %w", s.Name, err)
			}
			return nil
		}
	}
	deadline := time.Now().Add(timeout)
	for st.State != svc.Stopped {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s to stop", s.Name)
		}
		time.Sleep(200 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return err
		}
	}
	return nil
}

func startAndWait(s *mgr.Service, name string) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Running {
		return nil
	}
	if st.State == svc.Stopped {
		if err := s.Start(); err != nil {
			return fmt.Errorf("starting %s: %w", name, err)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if st, err = s.Query(); err != nil {
			return err
		}
		switch {
		case st.State == svc.Running:
			return nil
		case st.State == svc.Stopped:
			msg := fmt.Sprintf("%s stopped right after starting (exit code %d)", name, st.ServiceSpecificExitCode)
			if tail := lastLogLine(name); tail != "" {
				msg += ": " + tail
			}
			return errors.New(msg + "; log: " + LogPath(name))
		case time.Now().After(deadline):
			return fmt.Errorf("timed out waiting for %s to start", name)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func lastLogLine(name string) string {
	f, err := os.Open(LogPath(name))
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 4096 {
		f.Seek(-4096, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
