package mp

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/apernet/quic-go/quicvarint"
)

// Wire protocol (all integers are QUIC varints unless noted).
//
// Every path is a QUIC connection. The client opens one bidirectional
// control stream per connection and starts it with a HELLO; the server
// answers with HELLO_OK. Afterwards both sides exchange length-prefixed
// control frames on it:
//
//	frame := type:varint len:varint body[len]
//
// Data of a multipath stream ("mstream") is carried in unidirectional QUIC
// streams, one per (path, mstream, direction), opened lazily:
//
//	uni   := mstreamID:varint chunk*
//	chunk := offset:varint lenfin:varint data[lenfin>>1]   // fin = lenfin&1
//
// Offsets are data sequence numbers in the mstream byte space; FIN occupies
// one virtual byte at the final offset (like TCP). Chunks are immutable once
// created, so a chunk reinjected on another path is byte-identical and the
// receiver de-duplicates by offset.

const (
	alpn         = "mpquic/1"
	helloMagic   = "MPQ1"
	protoVersion = 1
)

const (
	frPing  = 1 // seq
	frPong  = 2 // seq
	frAck   = 3 // n, n*(id, cum, window, nranges, nranges*(start,end))
	frReset = 4 // id, code
	frDgram = 5 // raw datagram too large for a QUIC DATAGRAM frame
)

// Reset codes.
const (
	ResetGeneric     = 1
	ResetDialFailed  = 2
	ResetIdleTimeout = 3
	ResetSessionLost = 4
)

// Application error codes for QUIC CONNECTION_CLOSE.
const (
	errCodeAuth     = 0x101
	errCodeShutdown = 0x102
	errCodeStale    = 0x103
)

type ackRange struct{ start, end uint64 } // [start,end)

type ackEntry struct {
	id     uint64
	cum    uint64 // all bytes (incl. FIN virtual byte) below cum received
	window uint64 // sender may send data with offset+len <= window
	ranges []ackRange
}

// ackAll in cum means "the stream no longer exists here, everything is acked".
const ackAll = uint64(1) << 61

func appendFrame(b []byte, typ uint64, body []byte) []byte {
	b = quicvarint.Append(b, typ)
	b = quicvarint.Append(b, uint64(len(body)))
	return append(b, body...)
}

func encodeAck(entries []ackEntry) []byte {
	var body []byte
	body = quicvarint.Append(body, uint64(len(entries)))
	for _, e := range entries {
		body = quicvarint.Append(body, e.id)
		body = quicvarint.Append(body, e.cum)
		body = quicvarint.Append(body, e.window)
		body = quicvarint.Append(body, uint64(len(e.ranges)))
		for _, r := range e.ranges {
			body = quicvarint.Append(body, r.start)
			body = quicvarint.Append(body, r.end)
		}
	}
	return appendFrame(nil, frAck, body)
}

type byteReader struct {
	b []byte
	i int
}

func (r *byteReader) ReadByte() (byte, error) {
	if r.i >= len(r.b) {
		return 0, io.ErrUnexpectedEOF
	}
	c := r.b[r.i]
	r.i++
	return c, nil
}

func (r *byteReader) varint() (uint64, error) { return quicvarint.Read(r) }

func decodeAck(body []byte) ([]ackEntry, error) {
	r := &byteReader{b: body}
	n, err := r.varint()
	if err != nil || n > 1<<16 {
		return nil, errors.New("bad ack")
	}
	out := make([]ackEntry, 0, n)
	for i := uint64(0); i < n; i++ {
		var e ackEntry
		if e.id, err = r.varint(); err != nil {
			return nil, err
		}
		if e.cum, err = r.varint(); err != nil {
			return nil, err
		}
		if e.window, err = r.varint(); err != nil {
			return nil, err
		}
		nr, err := r.varint()
		if err != nil || nr > 256 {
			return nil, errors.New("bad ack ranges")
		}
		for j := uint64(0); j < nr; j++ {
			var a ackRange
			if a.start, err = r.varint(); err != nil {
				return nil, err
			}
			if a.end, err = r.varint(); err != nil {
				return nil, err
			}
			e.ranges = append(e.ranges, a)
		}
		out = append(out, e)
	}
	return out, nil
}

func encodeU64s(typ uint64, vals ...uint64) []byte {
	var body []byte
	for _, v := range vals {
		body = quicvarint.Append(body, v)
	}
	return appendFrame(nil, typ, body)
}

func decodeU64s(body []byte, n int) ([]uint64, error) {
	r := &byteReader{b: body}
	out := make([]uint64, n)
	for i := range out {
		v, err := r.varint()
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

const maxFrameLen = 1 << 20

func readFrame(r *bufio.Reader) (uint64, []byte, error) {
	typ, err := quicvarint.Read(r)
	if err != nil {
		return 0, nil, err
	}
	l, err := quicvarint.Read(r)
	if err != nil {
		return 0, nil, err
	}
	if l > maxFrameLen {
		return 0, nil, fmt.Errorf("control frame too large (%d)", l)
	}
	body := make([]byte, l)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}
