package gui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"mpquic/internal/config"
)

func TestRoundTrip(t *testing.T) {
	for _, js := range []string{
		`{"role":"local","server":["203.0.113.10","relay:9000"],"server_port":8388,"local_address":"127.0.0.1","local_port":1080,"password":"pw","method":"chacha20-ietf-poly1305","mode":"tcp_and_udp","timeout":300,"congestion":"bbr","window":"64MB","stats_interval":60}`,
		`{"role":"remote","server":"0.0.0.0","server_port":8388,"password":"pw","log_level":"debug","cert":"c.pem","key":"k.pem","no_dup":true,"mtu":1280}`,
		`{"role":"tunnel","server":["a:1"],"password":"pw","tunnel_address":"10.0.0.1:22","chunk_size":"8KB"}`,
		`{"role":"local","reverse":true,"password":"pw","local_port":1081}`,
		`{"role":"relay","relays":[{"listen":":9000","target":"h:8388"},{"listen":"0.0.0.0:9001","target":"h:8388","link":"delay=30ms,loss=0.5%","rev":"rate=10mbit"}]}`,
	} {
		var c config.Config
		if err := json.Unmarshal([]byte(js), &c); err != nil {
			t.Fatal(err)
		}
		got, err := FormFromConfig(&c).Apply(&c)
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		if !reflect.DeepEqual(got, &c) {
			a, _ := json.Marshal(got)
			t.Errorf("round trip changed the config:\n in: %s\nout: %s", js, a)
		}
	}
}

func TestApplyEdits(t *testing.T) {
	base := &config.Config{
		Servers:  []config.ServerEntry{{Server: "1.2.3.4", ServerPort: 443, Password: "p"}, {Server: "relay"}},
		Method:   "aes-256-gcm",
		Verbose:  true,
		FastOpen: true,
	}
	f := FormFromConfig(base)
	if f.Role != "local" || f.Paths != "1.2.3.4:443\nrelay" || f.Password != "p" || f.LogLevel != "debug" {
		t.Fatalf("form = %+v", f)
	}
	f.Paths += "\r\n\r\n # comment\r\n  10.0.0.1:9000  "
	f.LogLevel = "info"
	f.Window = "32mb"
	c, err := f.Apply(base)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]string(c.Server), []string{"1.2.3.4:443", "relay", "10.0.0.1:9000"}) || c.Servers != nil ||
		c.Password != "p" || c.Verbose || c.LogLevel != "" || c.Window != 32<<20 || c.Method != "aes-256-gcm" || !c.FastOpen {
		t.Fatalf("applied = %+v", c)
	}
	if len(base.Servers) != 2 || base.Server != nil {
		t.Fatal("base was modified")
	}
	for _, bad := range []func(*Form){
		func(f *Form) { f.Port = "x" },
		func(f *Form) { f.Window = "lots" },
		func(f *Form) { f.Password = "" },
		func(f *Form) { f.Paths = "" },
		func(f *Form) { f.Congestion = "vegas" },
		func(f *Form) { f.MTU = "1000" },
		func(f *Form) { f.MTU = "big" },
		func(f *Form) { f.Role = "tunnel" },
		func(f *Form) { f.Role = "relay"; f.Relays = "9000" },
	} {
		g := f
		bad(&g)
		if _, err := g.Apply(base); err == nil {
			t.Errorf("accepted %+v", g)
		}
	}
}

func TestRelays(t *testing.T) {
	rs, err := ParseRelays("9000 h:1\n:9001 h:2 delay=40ms fwd:loss=1% rev:rate=5mbit\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []config.RelayConfig{{Listen: ":9000", Target: "h:1"}, {Listen: ":9001", Target: "h:2", Link: "delay=40ms", Fwd: "loss=1%", Rev: "rate=5mbit"}}
	if !reflect.DeepEqual(rs, want) {
		t.Fatalf("relays = %+v", rs)
	}
	if s := FormatRelays(rs); !strings.Contains(s, ":9001 h:2 delay=40ms fwd:loss=1% rev:rate=5mbit") {
		t.Fatalf("format = %q", s)
	}
	if _, err := ParseRelays("a b c d"); err == nil {
		t.Fatal("expected error")
	}
}
