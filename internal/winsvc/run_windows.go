package winsvc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"mpquic/internal/app"
	"mpquic/internal/config"
	"mpquic/internal/logfile"
)

// Exit codes reported to the service control manager (service-specific).
const (
	exitConfig  = 2 // config missing or invalid
	exitFailure = 3 // a listener failed while running
)

// Run runs instance name. Started by the service control manager it runs
// as a service; started from a console it runs in the foreground with the
// same config and log file (useful for debugging), until Ctrl-C.
func Run(name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	// Connecting to the service manager is the reliable test: it fails
	// immediately when we were not started by it.
	err := svc.Run(name, &handler{name: name})
	if errors.Is(err, windows.ERROR_FAILED_SERVICE_CONTROLLER_CONNECT) {
		return runForeground(name)
	}
	return err
}

// tee writes to all writers, ignoring errors of all but the first (stderr
// may be invalid).
type tee []io.Writer

func (t tee) Write(p []byte) (int, error) {
	n, err := t[0].Write(p)
	for _, w := range t[1:] {
		w.Write(p)
	}
	return n, err
}

func loadConfig(name string) (*config.Config, string, error) {
	cfg, err := config.Load(ConfigPath(name))
	if err != nil {
		return nil, "", err
	}
	role, ok := app.NormalizeRole(cfg.Role)
	if !ok {
		return nil, "", fmt.Errorf("%s: missing or invalid \"role\" %q (local, tunnel, remote or relay)", ConfigPath(name), cfg.Role)
	}
	if err := app.Validate(role, cfg); err != nil {
		return nil, "", fmt.Errorf("%s: %w", ConfigPath(name), err)
	}
	return cfg, role, nil
}

func runForeground(name string) error {
	cfg, role, err := loadConfig(name)
	if err != nil {
		return err
	}
	var w io.Writer = os.Stderr
	if lf, err := logfile.Open(LogPath(name), 10<<20); err == nil {
		defer lf.Close()
		w = tee{lf, os.Stderr}
	}
	log := app.NewLogger(cfg, w)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return app.Run(ctx, role, cfg, log)
}

type handler struct{ name string }

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending, WaitHint: 10000}
	elog, _ := eventlog.Open(h.name)
	if elog != nil {
		defer elog.Close()
	}
	report := func(msg string) {
		if elog != nil {
			elog.Error(1, msg)
		}
	}

	var out io.Writer = io.Discard
	lf, lerr := logfile.Open(LogPath(h.name), 10<<20)
	if lerr == nil {
		defer lf.Close()
		out = lf
	} else {
		report("cannot open log file: " + lerr.Error())
	}
	cfg, role, err := loadConfig(h.name)
	if err != nil {
		fmt.Fprintf(out, "time=%s level=ERROR msg=\"service not started\" err=%q\n", time.Now().Format(time.RFC3339), err.Error())
		report("service not started: " + err.Error())
		return true, exitConfig
	}
	log := app.NewLogger(cfg, out)
	log.Info("service starting", "name", h.name, "role", role, "config", ConfigPath(h.name), "version", app.Version)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, role, cfg, log) }()

	// Listeners bind right away; report a start failure (e.g. a port in use)
	// as such instead of a service that dies right after running.
	select {
	case err := <-done:
		if err == nil {
			err = fmt.Errorf("%s exited unexpectedly", role)
		}
		log.Error("service not started", "err", err)
		report("service not started: " + err.Error())
		return true, exitFailure
	case <-time.After(750 * time.Millisecond):
	}
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			if err == nil {
				err = fmt.Errorf("%s exited unexpectedly", role)
			}
			log.Error("service failed", "err", err)
			report("service failed: " + err.Error())
			return true, exitFailure
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: 20000}
				cancel()
				select {
				case err := <-done:
					if err != nil {
						log.Warn("stopped with error", "err", err)
					}
				case <-time.After(15 * time.Second):
					log.Warn("shutdown timed out")
				}
				log.Info("service stopped")
				return false, 0
			}
		}
	}
}
