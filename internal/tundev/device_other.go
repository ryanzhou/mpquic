//go:build !linux && !darwin

package tundev

import (
	"errors"
	"net/netip"
	"runtime"

	"golang.zx2c4.com/wireguard/tun"
)

// DefaultName is the default name of the interface.
const DefaultName = "mpquic0"

func supported() error {
	return errors.New("the TUN interface is available on Linux and macOS, not on " + runtime.GOOS + "; use the WireGuard listener (--wg-listen) instead")
}

func checkName(string) error { return nil }

func openDevice(string, int) (tun.Device, error) { return nil, supported() }

func createHint(error) string { return "" }

func configure(string, []netip.Prefix, []netip.Prefix) error { return supported() }
