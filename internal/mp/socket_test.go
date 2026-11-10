package mp

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"
)

// Options.Control sees the sockets of the multipath connection.
func TestSocketControl(t *testing.T) {
	var calls atomic.Int32
	o := &Options{Control: func(network, address string, c syscall.RawConn) error {
		calls.Add(1)
		return nil
	}}
	c, err := o.listenUDP(context.Background(), "udp4", "")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if calls.Load() != 1 {
		t.Fatalf("Control called %d times for a path socket, want 1", calls.Load())
	}
	// an address needs no lookup
	a, err := o.resolveUDP(context.Background(), "192.0.2.7:8388")
	if err != nil || a.String() != "192.0.2.7:8388" {
		t.Fatalf("resolve: %v %v", a, err)
	}
	a, err = o.resolveUDP(context.Background(), "[2001:db8::1]:53")
	if err != nil || a.String() != "[2001:db8::1]:53" {
		t.Fatalf("resolve: %v %v", a, err)
	}
	if _, err := o.resolveUDP(context.Background(), "192.0.2.7"); err == nil {
		t.Fatal("an address without a port resolved")
	}
	if calls.Load() != 1 {
		t.Fatalf("Control called for resolving an IP address")
	}
}
