package mp

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/quicvarint"
)

// Authentication: TLS 1.3 (via QUIC) provides confidentiality and integrity.
// Peers authenticate each other with the pre-shared password by exchanging
// HMACs over the TLS exporter secret, which binds the proof to this very TLS
// session (a MITM terminating TLS on both sides sees different exporters and
// cannot forge the HMAC without the password). Certificates are therefore not
// verified; the server uses an ephemeral self-signed certificate.

const exporterLabel = "EXPORTER-mpquic-auth"

func deriveKey(password string) []byte {
	k, err := hkdf.Key(sha256.New, []byte(password), []byte("mpquic-v1-salt"), "mpquic psk", 32)
	if err != nil {
		panic(err)
	}
	return k
}

func authTag(key []byte, role string, exporter []byte, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(role))
	m.Write(exporter)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

func exporterOf(c *quic.Conn) ([]byte, error) {
	st := c.ConnectionState().TLS
	return st.ExportKeyingMaterial(exporterLabel, nil, 32)
}

func serverTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	var cert tls.Certificate
	var err error
	if certFile != "" {
		cert, err = tls.LoadX509KeyPair(certFile, keyFile)
	} else {
		cert, err = selfSigned()
	}
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{alpn},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func clientTLSConfig(sni string) *tls.Config {
	if sni == "" {
		sni = "mpquic"
	}
	return &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true, // authenticated by PSK-bound exporter HMAC
		NextProtos:         []string{alpn},
		MinVersion:         tls.VersionTLS13,
	}
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "mpquic"},
		DNSNames:     []string{"mpquic"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func quicConfig(o *Options) *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout:           o.HandshakeTimeout,
		MaxIdleTimeout:                 o.PathIdleTimeout,
		KeepAlivePeriod:                o.PathIdleTimeout / 3,
		InitialStreamReceiveWindow:     4 << 20,
		MaxStreamReceiveWindow:         uint64(o.Window),
		InitialConnectionReceiveWindow: 8 << 20,
		MaxConnectionReceiveWindow:     uint64(o.Window) * 4,
		MaxIncomingStreams:             16,
		MaxIncomingUniStreams:          1 << 14,
		EnableDatagrams:                true,
	}
}

type hello struct {
	session [16]byte
	index   uint64
	tag     []byte
}

func writeHello(w io.Writer, key, exporter []byte, session [16]byte, index uint64) error {
	b := []byte(helloMagic)
	b = append(b, protoVersion)
	b = append(b, session[:]...)
	b = quicvarint.Append(b, index)
	ib := quicvarint.Append(nil, index)
	b = append(b, authTag(key, "mpquic-client", exporter, session[:], ib)...)
	_, err := w.Write(b)
	return err
}

func readHello(r *bufio.Reader, key, exporter []byte) (*hello, error) {
	hdr := make([]byte, 5+16)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	if string(hdr[:4]) != helloMagic || hdr[4] != protoVersion {
		return nil, errors.New("bad hello magic/version")
	}
	h := &hello{}
	copy(h.session[:], hdr[5:])
	var err error
	if h.index, err = quicvarint.Read(r); err != nil {
		return nil, err
	}
	h.tag = make([]byte, 32)
	if _, err := io.ReadFull(r, h.tag); err != nil {
		return nil, err
	}
	want := authTag(key, "mpquic-client", exporter, h.session[:], quicvarint.Append(nil, h.index))
	if !hmac.Equal(want, h.tag) {
		return nil, errors.New("authentication failed")
	}
	return h, nil
}

func writeHelloOK(w io.Writer, key, exporter []byte, session [16]byte, nonce [8]byte) error {
	b := []byte(helloMagic)
	b = append(b, 0)
	b = append(b, nonce[:]...)
	b = append(b, authTag(key, "mpquic-server", exporter, session[:], nonce[:])...)
	_, err := w.Write(b)
	return err
}

func readHelloOK(r *bufio.Reader, key, exporter []byte, session [16]byte) ([8]byte, error) {
	var nonce [8]byte
	b := make([]byte, 5+8+32)
	if _, err := io.ReadFull(r, b); err != nil {
		return nonce, err
	}
	if string(b[:4]) != helloMagic || b[4] != 0 {
		return nonce, errors.New("bad hello reply")
	}
	copy(nonce[:], b[5:13])
	want := authTag(key, "mpquic-server", exporter, session[:], nonce[:])
	if !hmac.Equal(want, b[13:]) {
		return nonce, errors.New("server authentication failed (wrong password?)")
	}
	return nonce, nil
}
