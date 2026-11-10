package app

import (
	"runtime"
	"strings"
	"testing"

	"mpquic/internal/config"
)

func TestValidateTUN(t *testing.T) {
	base := func() config.Config {
		return config.Config{Server: config.StringList{"a"}, Password: "p"}
	}
	withTUN := func(tc config.TUNConfig, noSOCKS bool) config.Config {
		c := base()
		c.TUN, c.NoSOCKS = &tc, noSOCKS
		return c
	}
	hasTUN := runtime.GOOS == "linux" || runtime.GOOS == "darwin"
	c := withTUN(config.TUNConfig{Addresses: config.StringList{"10.99.0.1/24"}, Routes: config.StringList{"10.0.0.0/8"}}, true)
	if err := Validate("local", &c); (err == nil) != hasTUN {
		t.Errorf("TUN on %s: %v", runtime.GOOS, err)
	}
	c = base()
	c.FWMark = 0x4d51
	if err := Validate("local", &c); (err == nil) != (runtime.GOOS == "linux") {
		t.Errorf("fwmark on %s: %v", runtime.GOOS, err)
	}
	bad := []struct {
		role string
		cfg  config.Config
		want string
	}{
		{"remote", config.Config{Password: "p", TUN: &config.TUNConfig{}}, "local mode"},
		{"tunnel", func() config.Config {
			c := withTUN(config.TUNConfig{}, false)
			c.TunnelAddress = "h:22"
			return c
		}(), "local mode"},
		{"local", func() config.Config { c := base(); c.NoSOCKS = true; return c }(), "--tun"},
	}
	if hasTUN {
		bad = append(bad, []struct {
			role string
			cfg  config.Config
			want string
		}{
			{"local", withTUN(config.TUNConfig{MTU: 100}, false), "MTU"},
			{"local", withTUN(config.TUNConfig{Addresses: config.StringList{"nope"}}, false), "address"},
			{"local", withTUN(config.TUNConfig{Routes: config.StringList{"nope"}}, false), "route"},
		}...)
	}
	for _, b := range bad {
		err := Validate(b.role, &b.cfg)
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%s %+v: got %v, want an error about %q", b.role, b.cfg, err, b.want)
		}
	}
}
