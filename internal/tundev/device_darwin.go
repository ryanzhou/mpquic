package tundev

import (
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/tun"
)

// DefaultName is the default name of the interface: the system chooses the
// number.
const DefaultName = "utun"

func supported() error { return nil }

func checkName(name string) error {
	if name == "utun" {
		return nil
	}
	if n, ok := strings.CutPrefix(name, "utun"); ok {
		if _, err := strconv.ParseUint(n, 10, 16); err == nil {
			return nil
		}
	}
	return fmt.Errorf("TUN interface name %q: on macOS it is utun or utunN", name)
}

func openDevice(name string, mtu int) (tun.Device, error) { return tun.CreateTUN(name, mtu) }

func createHint(err error) string {
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		return " (creating a TUN interface needs root)"
	}
	return ""
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// configure assigns the addresses, brings the interface up and adds the
// routes, as wg-quick does. All of it goes away with the interface.
func configure(name string, addrs, routes []netip.Prefix) error {
	for _, a := range addrs {
		var err error
		if a.Addr().Is4() {
			// a point-to-point interface: the peer address is our own
			err = run("ifconfig", name, "inet", a.String(), a.Addr().String(), "alias")
		} else {
			err = run("ifconfig", name, "inet6", a.String(), "alias")
		}
		if err != nil {
			return err
		}
	}
	if err := run("ifconfig", name, "up"); err != nil {
		return err
	}
	for _, r := range routes {
		fam := "-inet"
		if r.Addr().Is6() {
			fam = "-inet6"
		}
		if err := run("route", "-q", "-n", "add", fam, r.String(), "-interface", name); err != nil {
			return err
		}
	}
	return nil
}
