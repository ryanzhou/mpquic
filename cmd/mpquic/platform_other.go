//go:build !windows

package main

import "errors"

func launchedFromExplorer() bool { return false }

func runService([]string) error {
	return errors.New("the service command is only available on Windows (use systemd or Docker elsewhere)")
}

func runGUI([]string) error {
	return errors.New("the configurator is only available on Windows")
}
