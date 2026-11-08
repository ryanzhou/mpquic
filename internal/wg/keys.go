package wg

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// Key is a Curve25519 key in WireGuard's base64 text form.
type Key [32]byte

// ParseKey parses a base64 WireGuard key (as printed by "wg genkey").
func ParseKey(s string) (Key, error) {
	var k Key
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != len(k) {
		return k, fmt.Errorf("invalid WireGuard key %q: want 32 bytes in base64", abbrev(s))
	}
	copy(k[:], b)
	return k, nil
}

func abbrev(s string) string {
	if len(s) > 12 {
		return s[:6] + "…"
	}
	return s
}

func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

func (k Key) hex() string { return hex.EncodeToString(k[:]) }

// PublicKey returns the public key of private key k.
func (k Key) PublicKey() Key {
	var pub Key
	p, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil { // only for low-order results, impossible with the base point
		panic(err)
	}
	copy(pub[:], p)
	return pub
}

// GenerateKey returns a new random private key.
func GenerateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return k, err
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	return k, nil
}
