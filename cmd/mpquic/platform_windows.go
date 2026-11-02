package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"mpquic/internal/app"
	"mpquic/internal/config"
	"mpquic/internal/gui"
	"mpquic/internal/winsvc"
)

// launchedFromExplorer reports whether we were started without a terminal
// (double-clicked): either no console at all (consoleAllocationPolicy
// "detached", Windows 11 24H2+), or a console that only we are attached to.
func launchedFromExplorer() bool { return gui.OwnsConsole() }

func runGUI(args []string) error {
	fs := flag.NewFlagSet("gui", flag.ContinueOnError)
	name := fs.String("name", winsvc.DefaultName, "service instance to configure")
	smoke := fs.Duration("smoke-test", 0, "open the window, round-trip the form and exit after this long (CI)")
	elevated := fs.Bool("elevated", false, "started through the UAC prompt (internal)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return gui.Run(gui.Options{Name: *name, Version: version, SmokeTest: *smoke, Elevated: *elevated})
}

func serviceUsage() {
	fmt.Fprint(os.Stderr, `Usage: mpquic service COMMAND [--name NAME] ...

  install [--name NAME] [--no-start] [ROLE [ROLE OPTIONS]]
        Install (or update) and start the service. ROLE is local, tunnel,
        remote or relay, followed by the same options as "mpquic ROLE", e.g.
          mpquic service install remote -p 8388 -k PASSWORD -u
          mpquic service install local -c C:\path\local.json
          mpquic service install --name relay relay --listen :9000 --target host:8388
        The config is stored in %ProgramData%\MPQUIC\NAME.json and the binary
        is copied to %ProgramFiles%\MPQUIC. Without ROLE, the stored config
        is reinstalled (e.g. after upgrading mpquic.exe).
  uninstall [--name NAME] [--purge]
        Stop and remove the service (--purge: also its config and logs).
  start | stop | restart | status [--name NAME]
  run [--name NAME]
        Run the instance (used by the service manager; from a console it runs
        in the foreground).

NAME defaults to MPQUIC. All commands except status need an elevated prompt.
`)
}

func runService(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		serviceUsage()
		return nil
	}
	cmd := args[0]
	fs := flag.NewFlagSet("service "+cmd, flag.ContinueOnError)
	fs.Usage = serviceUsage
	name := fs.String("name", winsvc.DefaultName, "service name")
	noStart := fs.Bool("no-start", false, "do not start the service after installing")
	purge := fs.Bool("purge", false, "uninstall: also delete config and logs")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := winsvc.CheckName(*name); err != nil {
		return err
	}
	progress := func(s string) { fmt.Println(s) }
	switch cmd {
	case "install":
		var cfg *config.Config
		if fs.NArg() > 0 {
			role, ok := app.NormalizeRole(fs.Arg(0))
			if !ok {
				return fmt.Errorf("unknown role %q (want local, tunnel, remote or relay)", fs.Arg(0))
			}
			c, err := parseRole(role, fs.Args()[1:])
			if err != nil {
				return err
			}
			c.Role = role
			cfg = c
		} else {
			c, err := config.Load(winsvc.ConfigPath(*name))
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("no stored config for %s; give a role and its options (see \"mpquic service help\")", *name)
				}
				return err
			}
			cfg = c
		}
		if err := winsvc.Install(winsvc.InstallOptions{Name: *name, Config: cfg, Start: !*noStart, Progress: progress}); err != nil {
			return err
		}
		st, _ := winsvc.Query(*name)
		fmt.Printf("%s is %s (log: %s)\n", *name, st, winsvc.LogPath(*name))
		return nil
	case "uninstall", "remove":
		return winsvc.Uninstall(*name, *purge, progress)
	case "start":
		return winsvc.Start(*name)
	case "stop":
		return winsvc.Stop(*name)
	case "restart":
		return winsvc.Restart(*name)
	case "status":
		st, err := winsvc.Query(*name)
		if err != nil {
			return err
		}
		fmt.Printf("name:    %s\nstatus:  %s\n", *name, st)
		if st.PID != 0 {
			fmt.Printf("pid:     %d\n", st.PID)
		}
		fmt.Printf("config:  %s\nlog:     %s\nbinary:  %s\n", winsvc.ConfigPath(*name), winsvc.LogPath(*name), winsvc.InstalledExe())
		if !st.Installed {
			os.Exit(3)
		}
		return nil
	case "run":
		return winsvc.Run(*name)
	}
	serviceUsage()
	return fmt.Errorf("unknown service command %q", cmd)
}
