package wg

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

var (
	c4 = netip.MustParseAddr("10.0.0.2")
	t4 = netip.MustParseAddr("198.51.100.7")
	c6 = netip.MustParseAddr("fd00::2")
	t6 = netip.MustParseAddr("2001:db8::7")
)

func taddr(a netip.Addr) tcpip.Address { return tcpip.AddrFromSlice(a.AsSlice()) }

// reassemble checks every fragment and returns the transport payload.
func reassemble(t *testing.T, pkts [][]byte, mtu int, proto uint8) []byte {
	t.Helper()
	var out []byte
	for i, p := range pkts {
		if len(p) > mtu {
			t.Fatalf("fragment %d: %d bytes > mtu %d", i, len(p), mtu)
		}
		last := i == len(pkts)-1
		if p[0]>>4 == 4 {
			h := header.IPv4(p)
			if !h.IsValid(len(p)) || !h.IsChecksumValid() {
				t.Fatalf("fragment %d: bad IPv4 header", i)
			}
			if int(h.FragmentOffset()) != len(out) || (h.Flags()&header.IPv4FlagMoreFragments != 0) == last || h.Protocol() != proto {
				t.Fatalf("fragment %d: offset %d more %v", i, h.FragmentOffset(), h.Flags())
			}
			out = append(out, h.Payload()...)
			continue
		}
		h := header.IPv6(p)
		if !h.IsValid(len(p)) {
			t.Fatalf("packet %d: bad IPv6 header", i)
		}
		if len(pkts) == 1 {
			if h.NextHeader() != proto {
				t.Fatalf("next header %d", h.NextHeader())
			}
			return h.Payload()
		}
		f := header.IPv6Fragment(h.Payload())
		if h.NextHeader() != ipv6Frag || !f.IsValid() || f.NextHeader() != proto || int(f.FragmentOffset())*8 != len(out) || f.More() == last {
			t.Fatalf("fragment %d: bad fragment header", i)
		}
		out = append(out, f.Payload()...)
	}
	return out
}

func TestUDPPackets(t *testing.T) {
	for _, tc := range []struct {
		src, dst netip.Addr
		size     int
	}{{t4, c4, 10}, {t4, c4, 1392}, {t4, c4, 5000}, {t6, c6, 10}, {t6, c6, 1372}, {t6, c6, 5000}} {
		payload := make([]byte, tc.size)
		for i := range payload {
			payload[i] = byte(i)
		}
		src, dst := netip.AddrPortFrom(tc.src, 53), netip.AddrPortFrom(tc.dst, 40000)
		pkts := udpPackets(src, dst, payload, DefaultMTU)
		limit := DefaultMTU - 28 // IPv4 + UDP headers
		if tc.src.Is6() {
			limit = DefaultMTU - 48
		}
		if wantFrag := tc.size > limit; (len(pkts) > 1) != wantFrag {
			t.Errorf("%v size %d: %d packets", tc.src, tc.size, len(pkts))
		}
		u := header.UDP(reassemble(t, pkts, DefaultMTU, protoUDP))
		if u.SourcePort() != 53 || u.DestinationPort() != 40000 || int(u.Length()) != len(u) || !bytes.Equal(u.Payload(), payload) {
			t.Fatalf("%v size %d: bad UDP datagram", tc.src, tc.size)
		}
		if !u.IsChecksumValid(taddr(tc.src), taddr(tc.dst), checksum.Checksum(u.Payload(), 0)) {
			t.Fatalf("%v size %d: bad UDP checksum", tc.src, tc.size)
		}
	}
}

func TestEcho(t *testing.T) {
	data := []byte("abcdefghijklmnopq") // odd length
	for _, pair := range [][2]netip.Addr{{c4, t4}, {c6, t6}} {
		client, target := pair[0], pair[1]
		reqType := uint8(8)
		if client.Is6() {
			reqType = 128
		}
		req := icmpPacket(client, target, reqType, 0, 0x1234_0007, data)
		e, isEcho, ok := parseEcho(req)
		if !isEcho || !ok || e.src != client || e.dst != target || e.id != 0x1234 || e.seq != 7 || !bytes.Equal(e.data, data) {
			t.Fatalf("parseEcho(%v) = %+v %v %v", client, e, isEcho, ok)
		}
		bad := bytes.Clone(req)
		bad[len(bad)-1] ^= 1
		if _, isEcho, ok := parseEcho(bad); !isEcho || ok {
			t.Fatal("corrupt echo request accepted")
		}
		// The reply is a valid echo reply (checked through parseEcho's
		// checksum logic by turning it back into a request).
		rep := echoReply(target, client, 0x1234, 7, data)
		hl := 20
		if client.Is6() {
			hl = 40
		}
		if want := uint8(0); client.Is6() && rep[hl] != 129 || client.Is4() && rep[hl] != want {
			t.Fatalf("reply type %d", rep[hl])
		}
		if client.Is4() {
			if !header.IPv4(rep).IsChecksumValid() || checksumFold(checksumAdd(0, rep[hl:])) != 0 {
				t.Fatal("bad ICMPv4 reply checksum")
			}
		} else if checksumFold(checksumAdd(pseudoHeaderSum(target, client, protoICMPv6, len(rep)-hl), rep[hl:])) != 0 {
			t.Fatal("bad ICMPv6 reply checksum")
		}
	}
	// Not echo requests: other ICMP, UDP, a later IPv4 fragment.
	unreach := icmpPacket(c4, t4, 3, 3, 0, nil)
	if _, isEcho, _ := parseEcho(unreach); isEcho {
		t.Fatal("destination unreachable taken for echo")
	}
	if _, isEcho, _ := parseEcho(udpPackets(netip.AddrPortFrom(c4, 1), netip.AddrPortFrom(t4, 2), data, 1420)[0]); isEcho {
		t.Fatal("UDP taken for echo")
	}
	// Fragmented echo requests are recognized (and dropped).
	big := make([]byte, 3000)
	m := make([]byte, 8, 8+len(big))
	m[0] = 8
	m = append(m, big...)
	frags := ipPackets(c4, t4, protoICMP, m, 1420)
	if _, isEcho, ok := parseEcho(frags[0]); !isEcho || ok {
		t.Fatal("first IPv4 fragment of an echo request not dropped")
	}
	if _, isEcho, _ := parseEcho(frags[1]); isEcho {
		t.Fatal("later fragment taken for echo")
	}
	m[0] = 128
	frags = ipPackets(c6, t6, protoICMPv6, m, 1420)
	if _, isEcho, ok := parseEcho(frags[0]); !isEcho || ok {
		t.Fatal("first IPv6 fragment of an echo request not dropped")
	}
}

func TestICMPErrors(t *testing.T) {
	orig := udpPackets(netip.AddrPortFrom(c4, 5000), netip.AddrPortFrom(t4, 443), make([]byte, 1300), 1420)[0]
	p := tooBig(t4, c4, orig, 1300)
	h := header.IPv4(p)
	if !h.IsChecksumValid() || h.SourceAddress() != taddr(t4) || h.DestinationAddress() != taddr(c4) {
		t.Fatal("bad IPv4 header")
	}
	m := header.ICMPv4(h.Payload())
	if m.Type() != header.ICMPv4DstUnreachable || m.Code() != header.ICMPv4FragmentationNeeded || m.MTU() != 1300 ||
		checksumFold(checksumAdd(0, m)) != 0 || !bytes.Equal(m.Payload(), orig[:28]) {
		t.Fatalf("bad fragmentation needed message %x", []byte(m[:8]))
	}
	orig6 := udpPackets(netip.AddrPortFrom(c6, 5000), netip.AddrPortFrom(t6, 443), make([]byte, 1300), 1420)[0]
	p = tooBig(t6, c6, orig6, 1300)
	if len(p) != 1280 {
		t.Fatalf("packet too big message is %d bytes, want 1280", len(p))
	}
	m6 := header.ICMPv6(header.IPv6(p).Payload())
	if m6.Type() != header.ICMPv6PacketTooBig || binary.BigEndian.Uint32(m6[4:]) != 1300 {
		t.Fatal("bad packet too big message")
	}
	p = unreachable(t6, c6, orig6)
	m6 = header.ICMPv6(header.IPv6(p).Payload())
	if m6.Type() != header.ICMPv6DstUnreachable || m6.Code() != header.ICMPv6PortUnreachable ||
		checksumFold(checksumAdd(pseudoHeaderSum(t6, c6, protoICMPv6, len(m6)), m6)) != 0 {
		t.Fatal("bad port unreachable message")
	}
}

func TestKeys(t *testing.T) {
	// RFC 7748 section 6.1 test vector.
	priv, err := ParseKey("dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LCo=")
	if err != nil {
		t.Fatal(err)
	}
	if got := priv.PublicKey().String(); got != "hSDwCYkwp1R0i33ctD73Wg2/Og0mOBr066SpjqqbTmo=" {
		t.Fatalf("public key %s", got)
	}
	for _, bad := range []string{"", "abc", "dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LC==", "dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LCoA"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) succeeded", bad)
		}
	}
	k, _ := GenerateKey()
	if k2, err := ParseKey(k.String()); err != nil || k2 != k || k[0]&7 != 0 || k[31]&0xc0 != 0x40 {
		t.Fatal("generated key does not round-trip or is not clamped")
	}
}

func TestCheck(t *testing.T) {
	k, _ := GenerateKey()
	pk := k.PublicKey().String()
	ok := Options{Listen: ":51820", PrivateKey: k.String(), Peers: []Peer{{PublicKey: pk}}}
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Options{
		"no key":         {Listen: ":1", Peers: ok.Peers},
		"no peers":       {Listen: ":1", PrivateKey: k.String()},
		"bad listen":     {Listen: "example.com:1", PrivateKey: k.String(), Peers: ok.Peers},
		"bad port":       {Listen: ":70000", PrivateKey: k.String(), Peers: ok.Peers},
		"bad allowed":    {Listen: ":1", PrivateKey: k.String(), Peers: []Peer{{PublicKey: pk, AllowedIPs: []string{"10.0.0.300"}}}},
		"two peers, any": {Listen: ":1", PrivateKey: k.String(), Peers: []Peer{{PublicKey: pk, AllowedIPs: []string{"10.0.0.2"}}, {PublicKey: pk}}},
		"mtu":            {Listen: ":1", PrivateKey: k.String(), Peers: ok.Peers, MTU: 500},
	} {
		if err := o.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
