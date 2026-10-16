package mp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apernet/quic-go"
)

var errSessionClosed = errors.New("session closed")

// Session groups all paths between one local and one remote instance and
// multiplexes streams across them.
type Session struct {
	opt      *Options
	log      *slog.Logger
	id       [16]byte
	isClient bool
	ctx      context.Context
	cancel   context.CancelFunc

	mu        sync.Mutex
	paths     []*Path
	nextPID   int
	streams   map[uint64]*Stream
	closedIDs map[uint64]time.Time
	nextID    uint64
	rr        []*Stream
	rrIdx     int
	unacked   map[*Stream]struct{}
	reinjectQ []*chunk
	backlog   int64
	ackDirty  map[*Stream]struct{}
	ackAllIDs []uint64
	ackSig    chan struct{}
	nonce     [8]byte
	hasNonce  bool
	acceptFn  func(*Stream)
	noPathAt  time.Time
	closed    bool
	tmpPaths  []*Path

	eagerTokens float64
	eagerLast   time.Time

	lastSlowTick  time.Time
	lastStatsTick time.Time
}

func newSession(opt *Options, id [16]byte, isClient bool, acceptFn func(*Stream)) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		opt:       opt,
		id:        id,
		isClient:  isClient,
		ctx:       ctx,
		cancel:    cancel,
		streams:   make(map[uint64]*Stream),
		closedIDs: make(map[uint64]time.Time),
		unacked:   make(map[*Stream]struct{}),
		ackDirty:  make(map[*Stream]struct{}),
		ackSig:    make(chan struct{}, 1),
		acceptFn:  acceptFn,
		nextID:    1,
		noPathAt:  time.Now(),

		lastStatsTick: time.Now(),
	}
	s.log = opt.Logger.With("session", hex.EncodeToString(id[:4]))
	if !isClient {
		rand.Read(s.nonce[:])
		s.hasNonce = true
	}
	go s.ackLoop()
	go s.tickLoop()
	return s
}

// OpenStream creates a new outgoing stream (client side).
func (s *Session) OpenStream() (*Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSessionClosed
	}
	id := s.nextID
	s.nextID++
	st := newStream(s, id)
	s.streams[id] = st
	return st, nil
}

// Close tears down the session, all its paths and streams.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for _, st := range s.streams {
		st.resetLocked(ResetSessionLost, false)
	}
	paths := append([]*Path(nil), s.paths...)
	s.mu.Unlock()
	for _, p := range paths {
		p.conn.CloseWithError(errCodeShutdown, "session closed")
	}
	s.cancel()
}

func (s *Session) addPath(p *Path, nonce *[8]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSessionClosed
	}
	if nonce != nil {
		if s.hasNonce && *nonce != s.nonce {
			n := 0
			for _, st := range s.streams {
				st.resetLocked(ResetSessionLost, false)
				n++
			}
			s.log.Warn("remote session state lost (server restarted or session expired); reset streams", "streams", n)
		}
		s.nonce = *nonce
		s.hasNonce = true
	}
	// A reconnect for the same configured index supersedes the old path.
	for _, q := range s.paths {
		if q.index == p.index && q.state != stateDead && s.isClient == false {
			q.conn.CloseWithError(errCodeStale, "superseded")
		}
	}
	s.nextPID++
	p.id = s.nextPID
	p.state = stateActive
	p.upSince = time.Now()
	s.paths = append(s.paths, p)
	s.wakeAllLocked()
	s.log.Info("path up", "path", p.name, "count", s.activeCountLocked())
	return nil
}

func (s *Session) removePath(p *Path, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.state == stateDead {
		return
	}
	p.state = stateDead
	for i, q := range s.paths {
		if q == p {
			s.paths = append(s.paths[:i], s.paths[i+1:]...)
			break
		}
	}
	if len(s.paths) == 0 {
		s.noPathAt = time.Now()
	}
	s.requeueLocked(time.Now())
	s.wakeAllLocked()
	s.log.Info("path down", "path", p.name, "err", errString(err), "count", s.activeCountLocked())
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Session) activeCountLocked() int {
	n := 0
	for _, p := range s.paths {
		if p.state == stateActive {
			n++
		}
	}
	return n
}

func (s *Session) wakeAll() { s.wakeAllLocked() }

func (s *Session) wakeAllLocked() {
	for _, p := range s.paths {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

func (s *Session) enqueueRR(st *Stream) {
	if !st.inRR {
		st.inRR = true
		s.rr = append(s.rr, st)
	}
}

func (s *Session) markAckLocked(st *Stream) {
	s.ackDirty[st] = struct{}{}
	select {
	case s.ackSig <- struct{}{}:
	default:
	}
}

// bestPathsLocked returns up to n usable paths ordered by smoothed RTT.
func (s *Session) bestPathsLocked(n int) []*Path {
	var act, sus []*Path
	for _, p := range s.paths {
		switch p.state {
		case stateActive:
			act = append(act, p)
		case stateSuspect:
			sus = append(sus, p)
		}
	}
	sort.Slice(act, func(i, j int) bool { return act[i].schedRTT() < act[j].schedRTT() })
	sort.Slice(sus, func(i, j int) bool { return sus[i].lastPong.After(sus[j].lastPong) })
	act = append(act, sus...)
	if len(act) > n {
		act = act[:n]
	}
	return act
}

func (s *Session) sendCtrlBest(b []byte, n int) {
	for _, p := range s.bestPathsLocked(n) {
		p.sendCtrl(b)
	}
}

func (s *Session) maybeFinalizeLocked(st *Stream) {
	if st.finalized || !st.appClosed {
		return
	}
	if st.err == nil && !(st.finAcked && st.recvFin) {
		return
	}
	st.finalized = true
	delete(s.streams, st.id)
	s.closedIDs[st.id] = time.Now()
	if st.inUnacked {
		delete(s.unacked, st)
		st.inUnacked = false
	}
	for _, p := range s.paths {
		p.closeIDs = append(p.closeIDs, st.id)
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// onChunk is called by path readers for every received data chunk.
func (s *Session) onChunk(p *Path, id, off uint64, data []byte, fin bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	now := time.Now()
	p.lastRecv = now
	st := s.streams[id]
	if st == nil {
		if _, ok := s.closedIDs[id]; ok || s.isClient || s.acceptFn == nil {
			s.ackAllIDs = append(s.ackAllIDs, id)
			select {
			case s.ackSig <- struct{}{}:
			default:
			}
			return
		}
		st = newStream(s, id)
		s.streams[id] = st
		go s.acceptFn(st)
	}
	st.lastActive = now
	st.onChunkLocked(off, data, fin)
}

func (s *Session) onCtrl(p *Path, typ uint64, body []byte) error {
	switch typ {
	case frPing:
		v, err := decodeU64s(body, 1)
		if err != nil {
			return err
		}
		p.sendCtrl(encodeU64s(frPong, v[0]))
		s.mu.Lock()
		p.lastRecv = time.Now()
		s.mu.Unlock()
	case frPong:
		v, err := decodeU64s(body, 1)
		if err != nil {
			return err
		}
		s.mu.Lock()
		p.onPongLocked(v[0], time.Now())
		s.mu.Unlock()
	case frAck:
		entries, err := decodeAck(body)
		if err != nil {
			return err
		}
		s.mu.Lock()
		p.lastRecv = time.Now()
		for i := range entries {
			if st := s.streams[entries[i].id]; st != nil {
				st.onAckLocked(&entries[i])
			}
		}
		s.wakeAllLocked()
		s.mu.Unlock()
	case frReset:
		v, err := decodeU64s(body, 2)
		if err != nil {
			return err
		}
		s.mu.Lock()
		if st := s.streams[v[0]]; st != nil {
			st.resetLocked(v[1], false)
		} else if !s.isClient {
			s.closedIDs[v[0]] = time.Now()
		}
		s.mu.Unlock()
	case frDgram:
		s.deliverDatagram(body)
	default:
		// unknown frames are ignored for forward compatibility
	}
	return nil
}

func (s *Session) ackLoop() {
	t := time.NewTimer(time.Hour)
	t.Stop()
	for {
		select {
		case <-s.ackSig:
		case <-s.ctx.Done():
			return
		}
		t.Reset(s.opt.AckDelay)
		select {
		case <-t.C:
		case <-s.ctx.Done():
			return
		}
		s.mu.Lock()
		entries := make([]ackEntry, 0, len(s.ackDirty)+len(s.ackAllIDs))
		for st := range s.ackDirty {
			entries = append(entries, st.ackEntryLocked())
			delete(s.ackDirty, st)
		}
		for _, id := range s.ackAllIDs {
			entries = append(entries, ackEntry{id: id, cum: ackAll})
		}
		s.ackAllIDs = s.ackAllIDs[:0]
		paths := s.bestPathsLocked(2)
		s.mu.Unlock()
		for len(entries) > 0 {
			n := len(entries)
			if n > 128 {
				n = 128
			}
			f := encodeAck(entries[:n])
			entries = entries[n:]
			for _, p := range paths {
				p.sendCtrl(f)
			}
		}
	}
}

const tickInterval = 10 * time.Millisecond

func (s *Session) tickLoop() {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-s.ctx.Done():
			return
		}
		s.tick(time.Now())
	}
}

func (s *Session) tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	active := len(s.streams) > 0
	changed := false
	for _, p := range s.paths {
		p.refreshStatsLocked()
		if p.tickLocked(now, active) {
			changed = true
		}
	}
	if s.reinjectOverdueLocked(now) {
		changed = true
	}
	if changed {
		s.requeueLocked(now)
		s.wakeAllLocked()
	}
	if now.Sub(s.lastSlowTick) >= time.Second {
		s.lastSlowTick = now
		for _, st := range s.streams {
			if now.Sub(st.lastActive) > s.opt.IdleTimeout {
				st.resetLocked(ResetIdleTimeout, true)
			} else if !s.isClient && st.recvOff == 0 && now.Sub(st.created) > 30*time.Second {
				st.resetLocked(ResetGeneric, true) // never received its beginning
			}
		}
		for id, at := range s.closedIDs {
			if now.Sub(at) > 3*time.Minute {
				delete(s.closedIDs, id)
			}
		}
	}
	if s.opt.StatsInterval > 0 && now.Sub(s.lastStatsTick) >= s.opt.StatsInterval {
		s.lastStatsTick = now
		s.log.Info("stats", "streams", len(s.streams), "backlog", s.backlog, "paths", s.statsStringLocked())
	}
}

// SendDatagram sends an unreliable datagram on the best path. Datagrams that
// do not fit in a QUIC DATAGRAM frame are sent reliably on the control stream.
func (s *Session) SendDatagram(b []byte) error {
	s.mu.Lock()
	paths := s.bestPathsLocked(1)
	s.mu.Unlock()
	if len(paths) == 0 {
		return errors.New("no path available")
	}
	p := paths[0]
	err := p.conn.SendDatagram(b)
	if err == nil {
		return nil
	}
	var tooLarge *quic.DatagramTooLargeError
	if errors.As(err, &tooLarge) {
		p.sendCtrl(appendFrame(nil, frDgram, b))
		return nil
	}
	return err
}

func (s *Session) deliverDatagram(b []byte) {
	if h := s.opt.DatagramHandler; h != nil {
		h(s, b)
	}
}

// Context is cancelled when the session is closed.
func (s *Session) Context() context.Context { return s.ctx }

// PathStat is a snapshot of one path, for monitoring.
type PathStat struct {
	Name      string
	State     string
	SRTT      time.Duration
	MinRTT    time.Duration
	Bandwidth uint64 // bytes/s (BBR estimate)
	Cwnd      uint64
	Pacing    uint64 // bytes/s
	CCMode    string
	SentBytes uint64
	RecvBytes uint64
	LostBytes uint64
}

// Stats returns per-path statistics.
func (s *Session) Stats() []PathStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PathStat
	for _, p := range s.paths {
		out = append(out, p.statLocked())
	}
	return out
}

func (s *Session) statsStringLocked() string {
	var b strings.Builder
	for i, p := range s.paths {
		if i > 0 {
			b.WriteString("; ")
		}
		st := p.statLocked()
		fmt.Fprintf(&b, "%s[%s rtt=%v bw=%.1fMbit/s tx=%dMB rx=%dMB lost=%dKB]", st.Name, st.State,
			st.SRTT.Round(time.Millisecond), float64(st.Bandwidth)*8/1e6, st.SentBytes>>20, st.RecvBytes>>20, st.LostBytes>>10)
	}
	return b.String()
}
