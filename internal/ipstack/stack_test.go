package ipstack

import (
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

func TestLossRecovery(t *testing.T) {
	s, err := New(Options{MTU: testMTU})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var rec tcpip.TCPRecovery
	if err := s.stack.TransportProtocolOption(tcp.ProtocolNumber, &rec); err != nil {
		t.Fatal(err)
	}
	if rec != 0 {
		t.Fatalf("TCP recovery %v, want 0 (classic SACK recovery, not RACK)", rec)
	}
}
