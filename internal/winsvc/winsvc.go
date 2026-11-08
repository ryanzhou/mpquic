// Package winsvc installs and runs mpquic as a Windows service.
//
// Layout (per service instance NAME, default "MPQUIC"):
//
//	%ProgramFiles%\MPQUIC\mpquic.exe        service binary (copied on install)
//	%ProgramData%\MPQUIC\NAME.json          config, incl. "role" (admins + LOCAL SERVICE read)
//	%ProgramData%\MPQUIC\logs\NAME.log      log (rotated to NAME.log.1)
//
// The service runs as NT AUTHORITY\LocalService. This file holds the
// platform-independent parts (names and firewall planning).
package winsvc

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"mpquic/internal/app"
	"mpquic/internal/config"
)

// DefaultName is the default service name.
const DefaultName = "MPQUIC"

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// CheckName validates a service instance name (it is also used in file names).
func CheckName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid service name %q (letters, digits, '.', '_' and '-', at most 64)", name)
	}
	return nil
}

// DisplayName is the name shown in services.msc.
func DisplayName(name string) string {
	if strings.EqualFold(name, DefaultName) {
		return "MPQUIC"
	}
	return "MPQUIC (" + name + ")"
}

// FirewallRule allows inbound traffic of one protocol to the service binary.
// Ports nil means any local port.
type FirewallRule struct {
	Protocol string // "TCP" or "UDP"
	Ports    []int
}

func (r FirewallRule) String() string {
	if r.Ports == nil {
		return r.Protocol + " (any port)"
	}
	return r.Protocol + " " + joinPorts(r.Ports)
}

func joinPorts(ports []int) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

// FirewallRules lists the inbound rules a config needs: the QUIC/relay UDP
// listeners, the WireGuard listener, and the SOCKS5/tunnel listener only
// when it is bound to a non-loopback address.
func FirewallRules(cfg *config.Config) []FirewallRule {
	role, _ := app.NormalizeRole(cfg.Role)
	var udp []string
	switch role {
	case "remote":
		if !cfg.Reverse {
			udp = cfg.ListenAddrs()
		}
	case "local", "tunnel":
		if cfg.Reverse {
			udp = cfg.ListenAddrs()
		}
	case "relay":
		for _, r := range cfg.Relays {
			udp = append(udp, r.Listen)
		}
	}
	if role == "local" && cfg.WireGuard != nil {
		l := cfg.WireGuard.WireGuardListen()
		if host, _, err := net.SplitHostPort(l); err == nil && (host == "" || !isLoopback(host)) {
			udp = append(udp, l) // "" is all interfaces here
		}
	}
	var rules []FirewallRule
	if ports := portsOf(udp); len(ports) > 0 {
		rules = append(rules, FirewallRule{"UDP", ports})
	}
	if (role == "local" || role == "tunnel") && !cfg.NoSOCKS && !isLoopback(cfg.LocalAddress) {
		ports := portsOf([]string{app.LocalListenAddr(cfg)})
		rules = append(rules, FirewallRule{"TCP", ports})
		if role == "local" && cfg.UDPEnabled() {
			// SOCKS5 UDP ASSOCIATE relays on ephemeral ports.
			rules = append(rules, FirewallRule{"UDP", nil})
		}
	}
	return mergeRules(rules)
}

func mergeRules(rules []FirewallRule) []FirewallRule {
	var out []FirewallRule
	for _, r := range rules {
		merged := false
		for i := range out {
			if out[i].Protocol != r.Protocol {
				continue
			}
			if out[i].Ports == nil || r.Ports == nil {
				out[i].Ports = nil
			} else {
				out[i].Ports = portsOf(nil, append(out[i].Ports, r.Ports...)...)
			}
			merged = true
		}
		if !merged {
			out = append(out, r)
		}
	}
	return out
}

// portsOf returns the sorted, deduplicated ports of host:port addresses
// (plus extra ports).
func portsOf(addrs []string, extra ...int) []int {
	seen := map[int]bool{}
	var out []int
	add := func(p int) {
		if p > 0 && p < 65536 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, a := range addrs {
		if _, ps, err := net.SplitHostPort(a); err == nil {
			p, _ := strconv.Atoi(ps)
			add(p)
		}
	}
	for _, p := range extra {
		add(p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func isLoopback(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
