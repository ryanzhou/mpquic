package app

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"mpquic/internal/config"
)

func TestValidate(t *testing.T) {
	ok := []struct {
		role string
		cfg  config.Config
	}{
		{"local", config.Config{Server: config.StringList{"a"}, Password: "p"}},
		{"ss-local", config.Config{Reverse: true, Password: "p"}},
		{"tunnel", config.Config{Server: config.StringList{"a"}, Password: "p", TunnelAddress: "h:22"}},
		{"server", config.Config{Password: "p", Congestion: "cubic"}},
		{"remote", config.Config{Reverse: true, Servers: []config.ServerEntry{{Server: "a", Password: "p"}}}},
		{"relay", config.Config{Relays: []config.RelayConfig{{Listen: ":9000", Target: "h:1", Link: "delay=10ms"}}}},
	}
	for _, c := range ok {
		if err := Validate(c.role, &c.cfg); err != nil {
			t.Errorf("%s %+v: %v", c.role, c.cfg, err)
		}
	}
	bad := []struct {
		role string
		cfg  config.Config
		want string
	}{
		{"bogus", config.Config{}, "unknown role"},
		{"local", config.Config{Server: config.StringList{"a"}}, "password"},
		{"local", config.Config{Password: "p"}, "server address"},
		{"tunnel", config.Config{Server: config.StringList{"a"}, Password: "p"}, "-L"},
		{"tunnel", config.Config{Server: config.StringList{"a"}, Password: "p", TunnelAddress: "nope"}, "tunnel target"},
		{"remote", config.Config{Password: "p", Reverse: true}, "reverse"},
		{"remote", config.Config{Password: "p", Congestion: "vegas"}, "congestion"},
		{"remote", config.Config{Password: "p", ServerPort: 70000}, "out of range"},
		{"remote", config.Config{Password: "p", Cert: "c.pem"}, "together"},
		{"remote", config.Config{Password: "p", Plugin: "v2ray"}, "plugin"},
		{"relay", config.Config{}, "no relays"},
		{"relay", config.Config{Relays: []config.RelayConfig{{Listen: "9000", Target: "h:1"}}}, "listen"},
		{"relay", config.Config{Relays: []config.RelayConfig{{Listen: ":9000", Target: "h:1", Rev: "bogus=1"}}}, "relay 1"},
	}
	for _, c := range bad {
		err := Validate(c.role, &c.cfg)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %+v: err = %v, want %q", c.role, c.cfg, err, c.want)
		}
	}
}

func freeUDPPort(t *testing.T) int {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func freeTCPPort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type running struct {
	cancel context.CancelFunc
	done   chan error
}

func start(role string, cfg *config.Config) *running {
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel, make(chan error, 1)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { r.done <- Run(ctx, role, cfg, log) }()
	return r
}

func (r *running) stop(t *testing.T, role string) {
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("%s: Run returned %v after cancel", role, err)
		}
	case <-time.After(10 * time.Second):
		t.Errorf("%s: Run did not return after cancel", role)
	}
}

// TestRunRoles runs remote, relay and local (direct + relay paths) and
// connects through the SOCKS5 proxy, then checks that cancellation stops
// all of them.
func TestRunRoles(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	qport, rport, sport := freeUDPPort(t), freeUDPPort(t), freeTCPPort(t)
	remote := start("remote", &config.Config{Server: config.StringList{"127.0.0.1"}, ServerPort: qport, Password: "pw"})
	defer remote.stop(t, "remote")
	relay := start("relay", &config.Config{Relays: []config.RelayConfig{
		{Listen: "127.0.0.1:" + strconv.Itoa(rport), Target: "127.0.0.1:" + strconv.Itoa(qport)}}})
	defer relay.stop(t, "relay")
	local := start("local", &config.Config{
		Server:   config.StringList{"127.0.0.1:" + strconv.Itoa(qport), "127.0.0.1:" + strconv.Itoa(rport)},
		Password: "pw", LocalPort: sport,
	})
	defer local.stop(t, "local")

	deadline := time.Now().Add(10 * time.Second)
	for {
		err := socksEcho("127.0.0.1:"+strconv.Itoa(sport), echo.Addr().(*net.TCPAddr))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socks echo: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// socksEcho opens a SOCKS5 CONNECT to target and checks an echo round trip.
func socksEcho(proxy string, target *net.TCPAddr) error {
	c, err := net.DialTimeout("tcp", proxy, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	req := []byte{5, 1, 0, 5, 1, 0, 1}
	req = append(req, target.IP.To4()...)
	req = binary.BigEndian.AppendUint16(req, uint16(target.Port))
	if _, err := c.Write(req); err != nil {
		return err
	}
	resp := make([]byte, 2+10)
	if _, err := io.ReadFull(c, resp); err != nil {
		return err
	}
	if resp[3] != 0 {
		return io.ErrUnexpectedEOF
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		return err
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// TestRunBindFailure checks that a role whose listener cannot bind returns
// an error instead of running without it (the service reports this).
func TestRunBindFailure(t *testing.T) {
	busy, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.LocalAddr().(*net.UDPAddr).Port
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, c := range []struct {
		role string
		cfg  *config.Config
	}{
		{"remote", &config.Config{Server: config.StringList{"127.0.0.1"}, ServerPort: port, Password: "pw"}},
		{"local", &config.Config{Reverse: true, Server: config.StringList{"127.0.0.1"}, ServerPort: port, Password: "pw", LocalPort: freeTCPPort(t)}},
		{"relay", &config.Config{Relays: []config.RelayConfig{{Listen: "127.0.0.1:" + strconv.Itoa(port), Target: "127.0.0.1:1"}}}},
	} {
		done := make(chan error, 1)
		go func() { done <- Run(context.Background(), c.role, c.cfg, log) }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: no error for a busy port", c.role)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: Run kept running on a busy port", c.role)
		}
	}
}
