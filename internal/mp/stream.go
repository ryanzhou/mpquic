package mp

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// ResetError is returned by Stream I/O after the stream was aborted.
type ResetError struct {
	Code   uint64
	Remote bool
}

func (e *ResetError) Error() string {
	who := "locally"
	if e.Remote {
		who = "by peer"
	}
	return fmt.Sprintf("stream reset %s (code %d)", who, e.Code)
}

var ErrWriteClosed = errors.New("write on closed stream")

type sendRec struct {
	p  *Path
	at time.Time
}

// chunk is an immutable (once sent) piece of an mstream.
type chunk struct {
	st     *Stream
	off    uint64
	data   []byte
	fin    bool
	sends  []sendRec
	acked  bool
	queued bool // in the reinjection queue
}

func (c *chunk) end() uint64 {
	e := c.off + uint64(len(c.data))
	if c.fin {
		e++
	}
	return e
}

func (c *chunk) sentOn(p *Path) bool {
	for _, r := range c.sends {
		if r.p == p {
			return true
		}
	}
	return false
}

// Stream is a reliable, ordered byte stream multiplexed over all paths of a
// session. It is safe for one reader and one writer goroutine.
type Stream struct {
	id   uint64
	sess *Session

	// ---- send side (guarded by sess.mu) ----
	sendBuf    []*chunk // unacked chunks in offset order
	nextUnsent int      // index in sendBuf of first never-sent chunk
	unsent     int64    // bytes in sendBuf[nextUnsent:]
	writeOff   uint64
	ackedOff   uint64 // data bytes cumulatively acked
	peerWindow uint64
	finQueued  bool
	finAcked   bool
	inRR       bool
	inUnacked  bool
	sendCond   *sync.Cond

	// ---- receive side ----
	recvOff       uint64   // next expected offset (incl. FIN virtual byte)
	ooo           []*chunk // out-of-order chunks, sorted by off, all > recvOff
	ready         [][]byte
	readyBytes    int64
	consumed      uint64
	lastAdvWindow uint64
	recvFin       bool
	recvCond      *sync.Cond

	err        error
	appClosed  bool
	finalized  bool
	lastActive time.Time
	created    time.Time
}

func newStream(s *Session, id uint64) *Stream {
	st := &Stream{id: id, sess: s, peerWindow: initialPeerWindow, lastActive: time.Now(), created: time.Now()}
	st.sendCond = sync.NewCond(&s.mu)
	st.recvCond = sync.NewCond(&s.mu)
	st.lastAdvWindow = uint64(s.opt.Window)
	return st
}

// ID returns the stream identifier.
func (st *Stream) ID() uint64 { return st.id }

// Write queues p for transmission. It blocks while the send buffer is full.
func (st *Stream) Write(p []byte) (int, error) {
	s := st.sess
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	cs := s.opt.ChunkSize
	for len(p) > 0 {
		for st.err == nil && !st.finQueued && int64(st.writeOff-st.ackedOff) >= s.opt.Window {
			st.sendCond.Wait()
		}
		if st.err != nil {
			return n, st.err
		}
		if st.finQueued {
			return n, ErrWriteClosed
		}
		room := s.opt.Window - int64(st.writeOff-st.ackedOff)
		sz := len(p)
		if int64(sz) > room {
			sz = int(room)
		}
		// Coalesce into the last chunk if it was never sent.
		if l := len(st.sendBuf); l > st.nextUnsent {
			last := st.sendBuf[l-1]
			if len(last.sends) == 0 && !last.fin && len(last.data) < cs {
				add := cs - len(last.data)
				if add > sz {
					add = sz
				}
				last.data = append(last.data, p[:add]...)
				st.appendAccounting(add)
				p = p[add:]
				n += add
				continue
			}
		}
		if sz > cs {
			sz = cs
		}
		buf := make([]byte, sz, cs)
		copy(buf, p[:sz])
		c := &chunk{st: st, off: st.writeOff, data: buf}
		st.sendBuf = append(st.sendBuf, c)
		st.appendAccounting(sz)
		p = p[sz:]
		n += sz
	}
	st.lastActive = time.Now()
	s.wakeAll()
	return n, nil
}

func (st *Stream) appendAccounting(n int) {
	st.writeOff += uint64(n)
	st.unsent += int64(n)
	st.sess.backlog += int64(n)
	st.sess.enqueueRR(st)
}

// CloseWrite sends FIN after all queued data.
func (st *Stream) CloseWrite() error {
	s := st.sess
	s.mu.Lock()
	defer s.mu.Unlock()
	return st.closeWriteLocked()
}

func (st *Stream) closeWriteLocked() error {
	if st.err != nil {
		return st.err
	}
	if st.finQueued {
		return nil
	}
	st.finQueued = true
	if l := len(st.sendBuf); l > st.nextUnsent && len(st.sendBuf[l-1].sends) == 0 {
		st.sendBuf[l-1].fin = true
	} else {
		st.sendBuf = append(st.sendBuf, &chunk{st: st, off: st.writeOff, fin: true})
	}
	st.unsent++
	st.sess.backlog++
	st.sess.enqueueRR(st)
	st.sess.wakeAll()
	return nil
}

// Read reads delivered in-order data. Returns io.EOF after FIN.
func (st *Stream) Read(p []byte) (int, error) {
	s := st.sess
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(st.ready) == 0 && st.err == nil && !st.recvFin {
		st.recvCond.Wait()
	}
	if len(st.ready) == 0 {
		if st.err != nil {
			return 0, st.err
		}
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && len(st.ready) > 0 {
		k := copy(p[n:], st.ready[0])
		n += k
		if k == len(st.ready[0]) {
			st.ready[0] = nil
			st.ready = st.ready[1:]
		} else {
			st.ready[0] = st.ready[0][k:]
		}
	}
	st.readyBytes -= int64(n)
	st.consumed += uint64(n)
	st.lastActive = time.Now()
	if st.consumed+uint64(s.opt.Window)-st.lastAdvWindow >= uint64(s.opt.Window)/4 {
		s.markAckLocked(st)
	}
	return n, nil
}

// Reset aborts the stream in both directions and notifies the peer.
func (st *Stream) Reset(code uint64) {
	s := st.sess
	s.mu.Lock()
	defer s.mu.Unlock()
	st.resetLocked(code, true)
}

// Close releases the stream. Unfinished send data is still delivered (FIN is
// queued if not done yet); the stream state is freed once everything is acked.
func (st *Stream) Close() error {
	s := st.sess
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.err == nil && !st.finQueued {
		st.closeWriteLocked()
	}
	st.appClosed = true
	st.ready = nil
	st.readyBytes = 0
	s.maybeFinalizeLocked(st)
	return nil
}

func (st *Stream) resetLocked(code uint64, notify bool) {
	if st.err != nil {
		return
	}
	s := st.sess
	st.err = &ResetError{Code: code, Remote: !notify}
	s.backlog -= st.unsent
	st.unsent = 0
	for _, c := range st.sendBuf {
		c.acked = true
	}
	st.sendBuf = nil
	st.nextUnsent = 0
	st.ready = nil
	st.readyBytes = 0
	st.ooo = nil
	if st.inUnacked {
		delete(s.unacked, st)
		st.inUnacked = false
	}
	st.sendCond.Broadcast()
	st.recvCond.Broadcast()
	if notify {
		s.sendCtrlBest(encodeU64s(frReset, st.id, code), 2)
	}
	s.maybeFinalizeLocked(st)
}

// onAck processes cumulative + selective acknowledgements (sender side).
func (st *Stream) onAckLocked(e *ackEntry) {
	s := st.sess
	if e.window > st.peerWindow {
		st.peerWindow = e.window
	}
	cum := e.cum
	popped := 0
	for popped < len(st.sendBuf) && st.sendBuf[popped].end() <= cum {
		c := st.sendBuf[popped]
		c.acked = true
		if popped >= st.nextUnsent { // acked before sent (ackAll)
			n := int64(len(c.data))
			if c.fin {
				n++
			}
			st.unsent -= n
			s.backlog -= n
		}
		st.ackedOff = c.off + uint64(len(c.data))
		if c.fin {
			st.finAcked = true
		}
		popped++
	}
	if popped > 0 {
		for i := 0; i < popped; i++ {
			st.sendBuf[i] = nil
		}
		st.sendBuf = st.sendBuf[popped:]
		st.nextUnsent -= popped
		if st.nextUnsent < 0 {
			st.nextUnsent = 0
		}
		st.sendCond.Broadcast()
	}
	if len(e.ranges) > 0 {
		ri := 0
		for i := 0; i < st.nextUnsent && ri < len(e.ranges); i++ {
			c := st.sendBuf[i]
			for ri < len(e.ranges) && e.ranges[ri].end < c.end() {
				ri++
			}
			if ri < len(e.ranges) && c.off >= e.ranges[ri].start && c.end() <= e.ranges[ri].end {
				c.acked = true
			}
		}
	}
	if cum >= ackAll && !st.finQueued {
		// Peer forgot the stream: nothing we write will ever be read.
		st.resetLocked(ResetGeneric, false)
		return
	}
	if st.nextUnsent == 0 && st.inUnacked {
		delete(s.unacked, st)
		st.inUnacked = false
	}
	s.maybeFinalizeLocked(st)
}

// onChunk handles received data (receiver side).
func (st *Stream) onChunkLocked(off uint64, data []byte, fin bool) {
	s := st.sess
	if st.err != nil || st.appClosed && st.recvFin {
		return
	}
	c := &chunk{off: off, data: data, fin: fin}
	end := c.end()
	s.markAckLocked(st)
	if end <= st.recvOff {
		return // duplicate
	}
	if off < st.recvOff { // overlap (should not happen with immutable chunks)
		c.data = c.data[st.recvOff-off:]
		c.off = st.recvOff
	}
	if c.off == st.recvOff {
		st.deliverLocked(c)
		for len(st.ooo) > 0 && st.ooo[0].off <= st.recvOff {
			n := st.ooo[0]
			st.ooo[0] = nil
			st.ooo = st.ooo[1:]
			if n.end() <= st.recvOff {
				continue
			}
			if n.off < st.recvOff {
				n.data = n.data[st.recvOff-n.off:]
				n.off = st.recvOff
			}
			st.deliverLocked(n)
		}
		return
	}
	i := sort.Search(len(st.ooo), func(i int) bool { return st.ooo[i].off >= c.off })
	if i < len(st.ooo) && st.ooo[i].off == c.off {
		return // duplicate
	}
	st.ooo = append(st.ooo, nil)
	copy(st.ooo[i+1:], st.ooo[i:])
	st.ooo[i] = c
}

func (st *Stream) deliverLocked(c *chunk) {
	if len(c.data) > 0 {
		if !st.appClosed {
			st.ready = append(st.ready, c.data)
			st.readyBytes += int64(len(c.data))
		} else {
			st.consumed += uint64(len(c.data))
		}
		st.recvOff += uint64(len(c.data))
	}
	if c.fin {
		st.recvOff++
		st.recvFin = true
	}
	st.recvCond.Broadcast()
	if st.recvFin {
		st.sess.maybeFinalizeLocked(st)
	}
}

func (st *Stream) ackEntryLocked() ackEntry {
	e := ackEntry{id: st.id, cum: st.recvOff, window: st.consumed + uint64(st.sess.opt.Window)}
	if st.err != nil || st.finalized {
		e.cum = ackAll
	}
	st.lastAdvWindow = e.window
	for _, c := range st.ooo {
		if n := len(e.ranges); n > 0 && e.ranges[n-1].end == c.off {
			e.ranges[n-1].end = c.end()
			continue
		}
		if len(e.ranges) >= 32 {
			break
		}
		e.ranges = append(e.ranges, ackRange{c.off, c.end()})
	}
	return e
}
