// Package proxy implements the shadowsocks-like application layer: a SOCKS5
// (or fixed-target tunnel) front-end on the local side and a TCP dialer on the
// remote side. Every proxied connection maps to one multipath stream whose
// first bytes are the target address in SOCKS5 format (as in shadowsocks).
package proxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

const (
	atypIPv4   = 1
	atypDomain = 3
	atypIPv6   = 4
)

// ReadAddr reads a SOCKS5-format address (ATYP, ADDR, PORT).
func ReadAddr(r io.Reader) (string, []byte, error) {
	var t [1]byte
	if _, err := io.ReadFull(r, t[:]); err != nil {
		return "", nil, err
	}
	return readAddrRest(r, t[0])
}

// readAddrRest reads the rest of a SOCKS5-format address of type atyp.
func readAddrRest(r io.Reader, atyp byte) (string, []byte, error) {
	var b [1 + 255 + 2]byte
	b[0] = atyp
	var host string
	n := 1
	switch b[0] {
	case atypIPv4:
		if _, err := io.ReadFull(r, b[1:1+4+2]); err != nil {
			return "", nil, err
		}
		host = net.IP(b[1:5]).String()
		n += 4
	case atypIPv6:
		if _, err := io.ReadFull(r, b[1:1+16+2]); err != nil {
			return "", nil, err
		}
		host = net.IP(b[1:17]).String()
		n += 16
	case atypDomain:
		if _, err := io.ReadFull(r, b[1:2]); err != nil {
			return "", nil, err
		}
		l := int(b[1])
		if _, err := io.ReadFull(r, b[2:2+l+2]); err != nil {
			return "", nil, err
		}
		host = string(b[2 : 2+l])
		n += 1 + l
	default:
		return "", nil, fmt.Errorf("unknown address type %d", b[0])
	}
	port := binary.BigEndian.Uint16(b[n : n+2])
	n += 2
	raw := append([]byte(nil), b[:n]...)
	return net.JoinHostPort(host, strconv.Itoa(int(port))), raw, nil
}

// addrLen returns the length of the SOCKS5-format address at the start of
// b, or -1 if b does not start with a complete one.
func addrLen(b []byte) int {
	if len(b) == 0 {
		return -1
	}
	n := -1
	switch b[0] {
	case atypIPv4:
		n = 1 + 4 + 2
	case atypIPv6:
		n = 1 + 16 + 2
	case atypDomain:
		if len(b) > 1 {
			n = 1 + 1 + int(b[1]) + 2
		}
	}
	if n > len(b) {
		return -1
	}
	return n
}

// EncodeAddr encodes host:port in SOCKS5 address format.
func EncodeAddr(hostport string) ([]byte, error) {
	host, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("bad port %q", ps)
	}
	var b []byte
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			b = append([]byte{atypIPv4}, v4...)
		} else {
			b = append([]byte{atypIPv6}, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, errors.New("hostname too long")
		}
		b = append([]byte{atypDomain, byte(len(host))}, host...)
	}
	return binary.BigEndian.AppendUint16(b, uint16(port)), nil
}

const (
	cmdConnect      = 1
	cmdUDPAssociate = 3
)

// socksHandshake performs the SOCKS5 greeting + request. It returns the
// command and the requested target in SOCKS5 address format.
func socksHandshake(c net.Conn, allowUDP bool) (byte, []byte, string, error) {
	var b [2]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return 0, nil, "", err
	}
	if b[0] != 5 {
		return 0, nil, "", fmt.Errorf("not SOCKS5 (version %d)", b[0])
	}
	methods := make([]byte, b[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return 0, nil, "", err
	}
	ok := false
	for _, m := range methods {
		if m == 0 {
			ok = true
		}
	}
	if !ok {
		c.Write([]byte{5, 0xff})
		return 0, nil, "", errors.New("client offers no acceptable auth method")
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return 0, nil, "", err
	}
	var h [3]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return 0, nil, "", err
	}
	if h[0] != 5 {
		return 0, nil, "", errors.New("bad request version")
	}
	target, raw, err := ReadAddr(c)
	if err != nil {
		return 0, nil, "", err
	}
	if h[1] != cmdConnect && !(h[1] == cmdUDPAssociate && allowUDP) {
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return 0, nil, "", fmt.Errorf("unsupported SOCKS command %d", h[1])
	}
	return h[1], raw, target, nil
}
