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
	protoVersion = 2
)

// QUIC DATAGRAM payloads start with a type byte.
const (
	dgUDP  = 0 // UDP relay payload (Session.SendDatagram)
	dgCtrl = 1 // one or more control frames (PING/PONG/ACK), unreliable
	dgData = 2 // one data chunk: dseq mstreamID off lenfin data (lossy paths)
)

// dgDataOverhead bounds the bytes a data datagram adds around the chunk data.
const dgDataOverhead = 1 + 8 + 8 + 8 + 4

// maxCtrlDatagram bounds a control datagram so it fits in one QUIC packet
// before PMTU discovery.
// (leaving room for a QUIC ACK frame in the same packet; quic-go silently
// drops DATAGRAMs that do not fit next to it).
const maxCtrlDatagram = 900

const (
	frPing  = 1 // seq
	frPong  = 2 // seq
	frAck   = 3 // n, n*(id, cum, window, nranges, nranges*(start,end))
	frReset = 4 // id, code
	// 5 was frDgram (an oversized UDP datagram sent reliably); unused now
	frPathAck = 6 // pathIndex, largest dseq, 32-byte bitmap of the 256 dseqs below
)

// pathAckBits is how many datagram sequence numbers below the largest one a
// PATHACK reports.
const pathAckBits = 256

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

// maxAckRanges bounds the SACK ranges per stream in one ACK. Ranges are
// delta-encoded (gap, length), typically 4 bytes each.
const maxAckRanges = 200

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

// encodeAckDatagrams packs ACK entries into control datagrams of at most
// maxCtrlDatagram bytes. An entry too large on its own keeps fewer SACK ranges.
func encodeAckDatagrams(entries []ackEntry) [][]byte {
	const budget = maxCtrlDatagram - 1 - 4 - 4 // type byte, frame type+len, count
	var out [][]byte
	var group [][]byte
	size := 0
	flush := func() {
		if len(group) == 0 {
			return
		}
		body := quicvarint.Append(nil, uint64(len(group)))
		for _, g := range group {
			body = append(body, g...)
		}
		out = append(out, appendFrame([]byte{dgCtrl}, frAck, body))
		group, size = nil, 0
	}
	for _, e := range entries {
		b := encodeAckEntry(e)
		for len(b) > budget && len(e.ranges) > 0 {
			e.ranges = e.ranges[:len(e.ranges)/2]
			b = encodeAckEntry(e)
		}
		if size+len(b) > budget {
			flush()
		}
		group = append(group, b)
		size += len(b)
	}
	flush()
	return out
}

func encodeAckEntry(e ackEntry) []byte {
	var b []byte
	b = quicvarint.Append(b, e.id)
	b = quicvarint.Append(b, e.cum)
	b = quicvarint.Append(b, e.window)
	n := 0
	prev := e.cum
	for _, r := range e.ranges {
		if e.cum >= ackAll || r.start < prev || r.end < r.start {
			break // not representable as deltas; never happens for real entries
		}
		prev = r.end
		n++
	}
	b = quicvarint.Append(b, uint64(n))
	prev = e.cum
	for _, r := range e.ranges[:n] {
		b = quicvarint.Append(b, r.start-prev)
		b = quicvarint.Append(b, r.end-r.start)
		prev = r.end
	}
	return b
}

func encodeAck(entries []ackEntry) []byte {
	body := quicvarint.Append(nil, uint64(len(entries)))
	for _, e := range entries {
		body = append(body, encodeAckEntry(e)...)
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
		if err != nil || nr > 1024 {
			return nil, errors.New("bad ack ranges")
		}
		prev := e.cum
		for j := uint64(0); j < nr; j++ {
			gap, err := r.varint()
			if err != nil {
				return nil, err
			}
			l, err := r.varint()
			if err != nil {
				return nil, err
			}
			a := ackRange{start: prev + gap}
			a.end = a.start + l
			prev = a.end
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
