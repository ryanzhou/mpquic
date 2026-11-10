package ipstack

import (
	"encoding/binary"
	"net/netip"
	"sync/atomic"
)

// Building the IP packets the WireGuard front-end sends to its peers for
// relayed UDP and ICMP (TCP goes through the gVisor stack).

const (
	protoICMP   = 1
	protoUDP    = 17
	protoICMPv6 = 58
	ipv6Frag    = 44
	defaultTTL  = 64
)

var ipID atomic.Uint32

// checksumAdd adds b to a one's complement sum.
func checksumAdd(sum uint32, b []byte) uint32 {
	for len(b) >= 2 {
		sum += uint32(b[0])<<8 | uint32(b[1])
		b = b[2:]
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}
	return sum
}

func checksumFold(sum uint32) uint16 {
	for sum > 0xffff {
		sum = sum>>16 + sum&0xffff
	}
	return ^uint16(sum)
}

func pseudoHeaderSum(src, dst netip.Addr, proto uint8, length int) uint32 {
	sum := checksumAdd(0, src.AsSlice())
	sum = checksumAdd(sum, dst.AsSlice())
	return sum + uint32(proto) + uint32(length)
}

// ipv4Header returns a 20-byte IPv4 header.
func ipv4Header(src, dst netip.Addr, proto uint8, payloadLen int, id uint16, fragField uint16) []byte {
	h := make([]byte, 20, 20+payloadLen)
	h[0] = 0x45
	binary.BigEndian.PutUint16(h[2:], uint16(20+payloadLen))
	binary.BigEndian.PutUint16(h[4:], id)
	binary.BigEndian.PutUint16(h[6:], fragField)
	h[8] = defaultTTL
	h[9] = proto
	s4, d4 := src.As4(), dst.As4()
	copy(h[12:16], s4[:])
	copy(h[16:20], d4[:])
	binary.BigEndian.PutUint16(h[10:], checksumFold(checksumAdd(0, h)))
	return h
}

// ipv6Header returns a 40-byte IPv6 header.
func ipv6Header(src, dst netip.Addr, next uint8, payloadLen int) []byte {
	h := make([]byte, 40, 40+payloadLen)
	h[0] = 0x60
	binary.BigEndian.PutUint16(h[4:], uint16(payloadLen))
	h[6] = next
	h[7] = defaultTTL
	s16, d16 := src.As16(), dst.As16()
	copy(h[8:24], s16[:])
	copy(h[24:40], d16[:])
	return h
}

// ipPackets wraps a transport payload (with a complete checksum) in IP
// packets of at most mtu bytes, fragmenting if needed.
func ipPackets(src, dst netip.Addr, proto uint8, payload []byte, mtu int) [][]byte {
	if src.Is4() {
		if 20+len(payload) <= mtu {
			return [][]byte{append(ipv4Header(src, dst, proto, len(payload), uint16(ipID.Add(1)), 0), payload...)}
		}
		id := uint16(ipID.Add(1))
		chunk := (mtu - 20) &^ 7
		var out [][]byte
		for off := 0; off < len(payload); off += chunk {
			end := min(off+chunk, len(payload))
			frag := uint16(off / 8)
			if end < len(payload) {
				frag |= 0x2000 // more fragments
			}
			out = append(out, append(ipv4Header(src, dst, proto, end-off, id, frag), payload[off:end]...))
		}
		return out
	}
	if 40+len(payload) <= mtu {
		return [][]byte{append(ipv6Header(src, dst, proto, len(payload)), payload...)}
	}
	id := ipID.Add(1)
	chunk := (mtu - 48) &^ 7
	var out [][]byte
	for off := 0; off < len(payload); off += chunk {
		end := min(off+chunk, len(payload))
		p := ipv6Header(src, dst, ipv6Frag, 8+end-off)
		fo := uint16(off) // offset in 8-byte units, shifted left by 3
		if end < len(payload) {
			fo |= 1
		}
		p = append(p, proto, 0, byte(fo>>8), byte(fo), byte(id>>24), byte(id>>16), byte(id>>8), byte(id))
		out = append(out, append(p, payload[off:end]...))
	}
	return out
}

// udpPackets builds the IP packet(s) of a UDP datagram from src to dst.
func udpPackets(src, dst netip.AddrPort, payload []byte, mtu int) [][]byte {
	u := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint16(u[0:], src.Port())
	binary.BigEndian.PutUint16(u[2:], dst.Port())
	binary.BigEndian.PutUint16(u[4:], uint16(8+len(payload)))
	u = append(u, payload...)
	sum := checksumAdd(pseudoHeaderSum(src.Addr(), dst.Addr(), protoUDP, len(u)), u)
	cs := checksumFold(sum)
	if cs == 0 {
		cs = 0xffff
	}
	binary.BigEndian.PutUint16(u[6:], cs)
	return ipPackets(src.Addr(), dst.Addr(), protoUDP, u, mtu)
}

// icmpPacket builds an ICMP (v4 or v6, by address family) packet with the
// given type, code, 4-byte rest-of-header and body.
func icmpPacket(src, dst netip.Addr, typ, code uint8, rest uint32, body []byte) []byte {
	m := make([]byte, 8, 8+len(body))
	m[0], m[1] = typ, code
	binary.BigEndian.PutUint32(m[4:], rest)
	m = append(m, body...)
	if src.Is4() {
		binary.BigEndian.PutUint16(m[2:], checksumFold(checksumAdd(0, m)))
		return append(ipv4Header(src, dst, protoICMP, len(m), uint16(ipID.Add(1)), 0), m...)
	}
	binary.BigEndian.PutUint16(m[2:], checksumFold(checksumAdd(pseudoHeaderSum(src, dst, protoICMPv6, len(m)), m)))
	return append(ipv6Header(src, dst, protoICMPv6, len(m)), m...)
}

// echoReply builds the reply to an echo request from client to target.
func echoReply(target, client netip.Addr, id, seq uint16, data []byte) []byte {
	typ := uint8(0) // ICMPv4 echo reply
	if target.Is6() {
		typ = 129
	}
	return icmpPacket(target, client, typ, 0, uint32(id)<<16|uint32(seq), data)
}

// tooBig builds the ICMP error telling client that orig (an IP packet it
// sent to target) exceeds mtu: "fragmentation needed" (IPv4) or "packet too
// big" (IPv6).
func tooBig(target, client netip.Addr, orig []byte, mtu int) []byte {
	if target.Is4() {
		ihl := int(orig[0]&0xf) * 4
		return icmpPacket(target, client, 3, 4, uint32(mtu), orig[:min(len(orig), ihl+8)])
	}
	// As much of the packet as fits into the IPv6 minimum MTU.
	return icmpPacket(target, client, 2, 0, uint32(mtu), orig[:min(len(orig), 1280-48)])
}

// unreachable builds the ICMP "port unreachable" error for orig, a packet
// from client to target.
func unreachable(target, client netip.Addr, orig []byte) []byte {
	if target.Is4() {
		ihl := int(orig[0]&0xf) * 4
		return icmpPacket(target, client, 3, 3, 0, orig[:min(len(orig), ihl+8)])
	}
	return icmpPacket(target, client, 1, 4, 0, orig[:min(len(orig), 1280-48)])
}

// echoRequest is a parsed ICMP echo request.
type echoRequest struct {
	src, dst netip.Addr
	id, seq  uint16
	data     []byte
}

// parseEcho classifies an IP packet received from a peer. isEcho reports an
// ICMP echo request; ok reports whether it can be relayed (it is not a
// fragment and its checksum is valid). Fragmented echo requests are dropped:
// they would otherwise reach gVisor, which answers pings itself.
func parseEcho(p []byte) (e echoRequest, isEcho, ok bool) {
	if len(p) < 1 {
		return e, false, false
	}
	switch p[0] >> 4 {
	case 4:
		if len(p) < 20 || p[9] != protoICMP {
			return e, false, false
		}
		ihl := int(p[0]&0xf) * 4
		total := int(binary.BigEndian.Uint16(p[2:]))
		frag := binary.BigEndian.Uint16(p[6:])
		if frag&0x1fff != 0 || ihl < 20 || total > len(p) || total < ihl+8 || p[ihl] != 8 {
			return e, false, false // not an echo request, or a later fragment
		}
		if frag&0x2000 != 0 {
			return e, true, false
		}
		m := p[ihl:total]
		if checksumFold(checksumAdd(0, m)) != 0 {
			return e, true, false
		}
		e.src = netip.AddrFrom4([4]byte(p[12:16]))
		e.dst = netip.AddrFrom4([4]byte(p[16:20]))
		e.id, e.seq, e.data = binary.BigEndian.Uint16(m[4:]), binary.BigEndian.Uint16(m[6:]), m[8:]
		return e, true, true
	case 6:
		if len(p) < 48 {
			return e, false, false
		}
		end := 40 + int(binary.BigEndian.Uint16(p[4:]))
		if end > len(p) || end < 48 {
			return e, false, false
		}
		switch p[6] {
		case protoICMPv6:
			if p[40] != 128 {
				return e, false, false
			}
			e.src = netip.AddrFrom16([16]byte(p[8:24]))
			e.dst = netip.AddrFrom16([16]byte(p[24:40]))
			m := p[40:end]
			if checksumFold(checksumAdd(pseudoHeaderSum(e.src, e.dst, protoICMPv6, len(m)), m)) != 0 {
				return e, true, false
			}
			e.id, e.seq, e.data = binary.BigEndian.Uint16(m[4:]), binary.BigEndian.Uint16(m[6:]), m[8:]
			return e, true, true
		case ipv6Frag:
			// First fragment of an echo request.
			if p[40] == protoICMPv6 && binary.BigEndian.Uint16(p[42:])&^7 == 0 && len(p) > 48 && p[48] == 128 {
				return e, true, false
			}
		}
	}
	return e, false, false
}
