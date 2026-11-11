package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go/quicvarint"

	"mpquic/internal/mp"
)

// Reliable UDP: every association is one stream, opened by the local side:
//
//	streamUDP  frame*
//	frame = len:varint  SOCKS5-address  payload   (len covers address + payload)
//
// Towards the remote the address is the datagram's target; back towards the
// local it is the peer the datagram came from. The stream gives the datagrams
// retransmission, multipath scheduling and congestion control, and has no
// size limit, at the cost of head-of-line blocking within the association.
//
// UDP senders cannot be slowed down, so instead of buffering up to the stream
// window (seconds of delay), a datagram is dropped when the stream's unsent
// backlog would take more than relMaxDelay to drain at the rate the stream
// has been draining: the tail drop of a router queue, at the tunnel's
// entrance rather than on the paths.

// streamUDP is the first byte of a reliable UDP stream (a TCP stream starts
// with a SOCKS5 address type instead).
const streamUDP = 0x55

const (
	relMaxDelay   = 200 * time.Millisecond
	relMinBacklog = 64 << 10 // never drop below this backlog
	relQueue      = 256      // datagrams waiting for the stream writer
	maxRelFrame   = 1 + 1 + 255 + 2 + 65535
)

var (
	errUDPCongested = errors.New("UDP datagram dropped: the tunnel is congested")
	errBadRelFrame  = errors.New("bad reliable UDP frame")
)

const relDropMsg = "reliable UDP: datagrams dropped, sent faster than the tunnel carries them"

func appendRelFrame(b, addr, payload []byte) []byte {
	b = quicvarint.Append(b, uint64(len(addr)+len(payload)))
	b = append(b, addr...)
	return append(b, payload...)
}

// relClock measures how fast a stream's unsent backlog drains. It is used by
// the stream's single writer.
type relClock struct {
	written  uint64
	t0       time.Time // start of the current sample (zero: none)
	sent0    uint64
	rate     float64 // bytes/s, while backlogged
	haveRate bool
	rateAt   time.Time // the last sample
}

// relRateAge is how long a drain rate stays valid without a new sample.
const relRateAge = time.Second

const relSample = 50 * time.Millisecond

func (c *relClock) wrote(n int) { c.written += uint64(n) }

// congested reports whether the next datagram for st is to be dropped.
func (c *relClock) congested(st *mp.Stream, now time.Time) bool {
	unsent := st.Unsent()
	sent := c.written - uint64(unsent)
	switch {
	case unsent < relMinBacklog/4:
		// not backlogged: the drain rate is the sender's, not the paths'
		c.t0 = time.Time{}
		// An old rate says nothing about the paths now: one measured
		// during an outage would otherwise drop every later burst, which
		// drains too fast to be sampled again.
		if c.haveRate && now.Sub(c.rateAt) > relRateAge {
			c.haveRate = false
		}
	case c.t0.IsZero():
		c.t0, c.sent0 = now, sent
	case now.Sub(c.t0) >= relSample:
		r := float64(sent-c.sent0) / now.Sub(c.t0).Seconds()
		if c.haveRate {
			c.rate += (r - c.rate) / 4
		} else {
			c.rate, c.haveRate = r, true
		}
		c.rateAt = now
		c.t0, c.sent0 = now, sent
	}
	if unsent <= relMinBacklog || !c.haveRate {
		return false
	}
	return float64(unsent) > c.rate*relMaxDelay.Seconds()
}

// readRelFrames reads frames from r and passes each one's address + payload
// (address length al) to deliver, which must not keep msg. It returns io.EOF
// at the end of the stream.
func readRelFrames(r io.Reader, deliver func(msg []byte, al int)) error {
	br := bufio.NewReaderSize(r, 16<<10)
	buf := make([]byte, 2048)
	for {
		n, err := quicvarint.Read(br)
		if err != nil {
			return err
		}
		if n > maxRelFrame {
			return errBadRelFrame
		}
		if int(n) > len(buf) {
			buf = make([]byte, n)
		}
		msg := buf[:n]
		if _, err := io.ReadFull(br, msg); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		al := addrLen(msg)
		if al < 0 {
			return errBadRelFrame
		}
		deliver(msg, al)
	}
}

// rateLog counts events and warns about them at most every 30 s.
type rateLog struct {
	mu sync.Mutex
	n  uint64
	at time.Time
}

func (r *rateLog) note(log *slog.Logger, msg string) {
	now := time.Now()
	r.mu.Lock()
	r.n++
	n := r.n
	warn := now.Sub(r.at) >= 30*time.Second
	if warn {
		r.at = now
	}
	r.mu.Unlock()
	if warn {
		log.Warn(msg, "total", n)
	}
}

// relAssoc is the local side of a reliable association. Its stream is opened
// on the first datagram, and again after a failure (e.g. an idle timeout).
type relAssoc struct {
	u       *UDPLocal
	id      uint64
	deliver func([]byte)
	q       chan []byte
	done    chan struct{}
	once    sync.Once
	st      atomic.Pointer[mp.Stream] // nil: none (yet, or it failed)
}

func newRelAssoc(u *UDPLocal, id uint64, deliver func([]byte)) *relAssoc {
	a := &relAssoc{u: u, id: id, deliver: deliver, q: make(chan []byte, relQueue), done: make(chan struct{})}
	go a.run()
	return a
}

func (a *relAssoc) send(target, payload []byte) error {
	select {
	case <-a.done:
		return net.ErrClosed
	default:
	}
	select {
	case a.q <- appendRelFrame(make([]byte, 0, 4+len(target)+len(payload)), target, payload):
		return nil
	default:
		a.u.drops.note(a.u.Log, relDropMsg)
		return errUDPCongested
	}
}

func (a *relAssoc) close() { a.once.Do(func() { close(a.done) }) }

// run writes the queued frames to the stream.
func (a *relAssoc) run() {
	var st *mp.Stream
	var clk relClock
	defer func() {
		if st != nil {
			// FIN: the remote releases its socket and ends the stream,
			// whereupon read closes it.
			st.CloseWrite()
		}
	}()
	for {
		var f []byte
		select {
		case <-a.done:
			return
		case f = <-a.q:
		}
		if st != nil && a.st.Load() != st { // the reader saw it fail
			st.Close()
			st = nil
		}
		if st == nil {
			s, err := a.u.Client.OpenStream()
			if err != nil {
				a.u.Log.Debug("udp stream", "id", a.id, "err", err)
				continue
			}
			st, clk = s, relClock{}
			a.st.Store(st)
			go a.read(st)
			a.u.Log.Debug("udp stream", "id", a.id, "stream", st.ID())
			f = append([]byte{streamUDP}, f...)
		}
		now := time.Now()
		if clk.congested(st, now) {
			a.u.drops.note(a.u.Log, relDropMsg)
			continue
		}
		clk.wrote(len(f))
		if _, err := st.Write(f); err != nil {
			a.u.Log.Debug("udp stream write", "id", a.id, "err", err)
			st.Reset(mp.ResetGeneric)
			st.Close()
			a.st.CompareAndSwap(st, nil)
			st = nil
		}
	}
}

// read delivers the datagrams arriving on st.
func (a *relAssoc) read(st *mp.Stream) {
	got := false
	err := readRelFrames(st, func(msg []byte, _ int) {
		got = true
		a.deliver(msg)
	})
	a.st.CompareAndSwap(st, nil)
	var re *mp.ResetError
	if errors.As(err, &re) && re.Remote && !got && (re.Code == mp.ResetGeneric || re.Code == mp.ResetDialFailed) {
		a.u.refused.note(a.u.Log, "reliable UDP stream refused by the remote: it needs UDP relaying (-u) and a version with reliable UDP")
	}
	if err != io.EOF {
		st.Reset(mp.ResetGeneric)
	}
	st.Close()
}

// serveUDPStream relays a reliable UDP association (remote side) through a
// UDP socket of its own.
func serveUDPStream(log *slog.Logger, st *mp.Stream, drops *rateLog) {
	pc, err := net.ListenUDP("udp", nil)
	if err != nil {
		log.Debug("udp stream", "stream", st.ID(), "err", err)
		st.Reset(mp.ResetDialFailed)
		st.Close()
		return
	}
	log.Debug("udp stream", "stream", st.ID(), "bind", pc.LocalAddr().String())
	done := make(chan struct{})
	go func() { // datagrams from the peers to the local
		defer close(done)
		buf := make([]byte, 65536)
		var frame []byte
		var clk relClock
		for {
			n, from, err := pc.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			now := time.Now()
			if clk.congested(st, now) {
				drops.note(log, relDropMsg)
				continue
			}
			frame = appendRelFrame(frame[:0], AppendAddrPort(nil, from), buf[:n])
			clk.wrote(len(frame))
			if _, err := st.Write(frame); err != nil {
				pc.Close() // the stream failed: end the association
				return
			}
		}
	}()
	resolved := make(map[string]*net.UDPAddr)
	err = readRelFrames(st, func(msg []byte, al int) {
		ua := resolved[string(msg[:al])]
		if ua == nil {
			target, _, err := ReadAddr(bytes.NewReader(msg[:al]))
			if err == nil {
				ua, err = net.ResolveUDPAddr("udp", target)
			}
			if err != nil {
				log.Debug("udp resolve", "target", target, "err", err)
				return
			}
			if len(resolved) >= 1024 {
				clear(resolved)
			}
			resolved[string(msg[:al])] = ua
		}
		pc.WriteToUDP(msg[al:], ua)
	})
	pc.Close()
	<-done
	if err != io.EOF {
		st.Reset(mp.ResetGeneric)
	}
	st.Close()
}
