package tundev

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/tun"
)

// DefaultName is the default name of the interface.
const DefaultName = "mpquic0"

func supported() error { return nil }

func checkName(name string) error {
	if len(name) >= unix.IFNAMSIZ {
		return fmt.Errorf("TUN interface name %q: longer than %d characters", name, unix.IFNAMSIZ-1)
	}
	for _, c := range name {
		if c <= ' ' || c == '/' || c == ':' || c > '~' {
			return fmt.Errorf("TUN interface name %q: invalid character", name)
		}
	}
	return nil
}

func openDevice(name string, mtu int) (tun.Device, error) { return tun.CreateTUN(name, mtu) }

func createHint(err error) string {
	switch {
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
		return " (creating a TUN interface needs root or CAP_NET_ADMIN)"
	case errors.Is(err, unix.EBUSY):
		return " (the interface is in use by another process)"
	case errors.Is(err, os.ErrNotExist):
		return " (no TUN support: load the tun kernel module; in a container, pass /dev/net/tun)"
	}
	return ""
}

// routeIsOurs reports whether the main table's route to rt's destination
// goes through link.
func routeIsOurs(link netlink.Link, rt *netlink.Route) bool {
	fam := netlink.FAMILY_V6
	if rt.Dst.IP.To4() != nil {
		fam = netlink.FAMILY_V4
	}
	routes, err := netlink.RouteListFiltered(fam, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return false
	}
	want, _ := rt.Dst.Mask.Size()
	for _, r := range routes {
		if r.LinkIndex != link.Attrs().Index {
			continue
		}
		if r.Dst == nil { // the default route
			if want == 0 {
				return true
			}
			continue
		}
		if ones, _ := r.Dst.Mask.Size(); ones == want && r.Dst.IP.Equal(rt.Dst.IP) {
			return true
		}
	}
	return false
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

// configure assigns the addresses, brings the interface up and adds the
// routes. All of it goes away with the interface.
func configure(name string, addrs, routes []netip.Prefix) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: ipNet(a)}); err != nil {
			return fmt.Errorf("address %s: %w", a, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set up: %w", err)
	}
	for _, r := range routes {
		rt := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: ipNet(r)}
		if r.Addr().Is4() {
			rt.Scope = netlink.SCOPE_LINK
		}
		// Add, not replace: a route of the system to the same
		// destination (its default route, say) would be gone for good,
		// also after this process has exited.
		if err := netlink.RouteAdd(rt); err != nil {
			if errors.Is(err, unix.EEXIST) {
				if routeIsOurs(link, rt) {
					continue // a persistent interface: from an earlier run
				}
				return fmt.Errorf("route %s: the system already has a route to this destination, which --tun-route does not replace "+
					"(use more specific routes, or a routing table of your own: see docs/tun.md)", r)
			}
			return fmt.Errorf("route %s: %w", r, err)
		}
	}
	return nil
}
