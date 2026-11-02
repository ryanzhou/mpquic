package winsvc

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

func firewallRuleName(name string) string { return DisplayName(name) }

// ApplyFirewall replaces the instance's Windows Firewall rules.
func ApplyFirewall(name, exe string, rules []FirewallRule) error {
	RemoveFirewall(name)
	for _, r := range rules {
		args := []string{"advfirewall", "firewall", "add", "rule",
			"name=" + firewallRuleName(name), "dir=in", "action=allow", "enable=yes", "profile=any",
			"program=" + exe, "protocol=" + r.Protocol,
			"description=Inbound " + r.String() + " for the " + DisplayName(name) + " service"}
		if r.Ports != nil {
			args = append(args, "localport="+joinPorts(r.Ports))
		}
		if out, err := netsh(args...); err != nil {
			return fmt.Errorf("firewall rule %s: %v: %s", r, err, strings.TrimSpace(out))
		}
	}
	return nil
}

// RemoveFirewall deletes the instance's firewall rules (if any).
func RemoveFirewall(name string) {
	netsh("advfirewall", "firewall", "delete", "rule", "name="+firewallRuleName(name))
}

func netsh(args ...string) (string, error) {
	cmd := exec.Command("netsh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.CombinedOutput()
	return string(out), err
}
