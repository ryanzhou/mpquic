package mp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"mpquic/internal/cc"

	"github.com/apernet/quic-go"
)

var errSessionClosed = errors.New("session closed")

var debugWindows = os.Getenv("MPQUIC_DEBUG_WINDOWS") != ""

const holDebugThreshold = 50 * time.Millisecond

// Session groups all paths between one local and one remote instance and
// multiplexes streams across them.
type Session struct {
	opt *Options
	log *slog.Logger
	id  [16]byte
	// dialer is the QUIC role: the dialer authenticates the listener's
	// session nonce; the listener generates it. Which side opens streams is
	// independent: a session accepts incoming streams iff acceptFn != nil.
	dialer bool
	ctx    context.Context
	cancel context.CancelFunc

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
	probeGate cc.ProbeGate // one path at a time in BBR PROBE_RTT
	backlog   int64
	ackDirty  map[*Stream]struct{}
	ackAllIDs []uint64
	ackSig    chan struct{}
	nonce     [8]byte
	hasNonce  bool
	acceptFn  func(*Stream)
	noPathAt  time.Time
	created   time.Time

	oversizeDrops  int // UDP datagrams dropped as too large (SendDatagram)
	oversizeWarnAt time.Time
	closed         bool
	tmpPaths       []*Path

	eagerTokens float64
	eagerLast   time.Time

	sb sbState // shared bottlenecks (shared.go)

	lastSlowTick  time.Time
	lastReAck     time.Time
	lastStatsTick time.Time
}

func newSession(opt *Options, id [16]byte, dialer bool, acceptFn func(*Stream)) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		opt:       opt,
		id:        id,
		dialer:    dialer,
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
		created:   time.Now(),

		lastStatsTick: time.Now(),
	}
	s.log = opt.Logger.With("session", hex.EncodeToString(id[:4]))
	if !dialer {
		rand.Read(s.nonce[:])
		s.hasNonce = true
	}
	go s.ackLoop()
	go s.tickLoop()
	return s
}

// ActivePaths returns the number of usable paths.
func (s *Session) ActivePaths() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeCountLocked()
}

// OpenStream creates a new outgoing stream.
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
		if q.index == p.index && q.state != stateDead && !s.dialer {
			q.conn.CloseWithError(errCodeStale, "superseded")
		}
	}
	s.nextPID++
	p.id = s.nextPID
	p.state = stateActive
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
	if p.meter != nil {
		s.probeGate.Release(p.meter)
	}
	for i, q := range s.paths {
		if q == p {
			s.paths = append(s.paths[:i], s.paths[i+1:]...)
			break
		}
	}
	if len(s.paths) == 0 {
		s.noPathAt = time.Now()
	}
	s.updateBenchedLocked()
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

// maxRTTLocked is the largest scheduling RTT among usable paths (the span
// over which data of one stream can be in flight / reordered).
func (s *Session) maxRTTLocked() time.Duration {
	r := 50 * time.Millisecond
	for _, p := range s.paths {
		if p.state == stateActive {
			r = max(r, p.srttOrDefault())
		}
	}
	return r
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

// updateBenchedLocked recomputes which paths sit out (see Path.benchedLocked).
func (s *Session) updateBenchedLocked() {
	changed := false
	for _, p := range s.paths {
		if b := p.benchedLocked(); b != p.benched {
			p.benched = b
			changed = true
		}
	}
	if changed {
		s.wakeAllLocked()
	}
}

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

// ackPathsLocked picks the paths that carry ACKs: the two with the lowest
// expected delivery time of a small datagram (accounting for loss).
func (s *Session) ackPathsLocked() []*Path {
	now := time.Now()
	var act []*Path
	for _, p := range s.paths {
		if p.usable() {
			act = append(act, p)
		}
	}
	if len(act) == 0 {
		return s.bestPathsLocked(2)
	}
	sort.Slice(act, func(i, j int) bool { return act[i].est(64, now) < act[j].est(64, now) })
	if len(act) > 2 {
		act = act[:2]
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

// onDgramChunk handles a data chunk received as a datagram.
func (s *Session) onDgramChunk(p *Path, dseq, id, off uint64, data []byte, fin bool) {
	s.mu.Lock()
	p.rxMarkLocked(dseq, time.Now())
	select {
	case s.ackSig <- struct{}{}:
	default:
	}
	s.mu.Unlock()
	s.onChunk(p, id, off, data, fin)
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
		if _, ok := s.closedIDs[id]; ok || s.acceptFn == nil {
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
		p.sendDgram(append([]byte{dgCtrl}, encodeU64s(frPong, v[0])...))
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
		} else if s.acceptFn != nil {
			s.closedIDs[v[0]] = time.Now()
		}
		s.mu.Unlock()
	case frPathAck:
		r := &byteReader{b: body}
		idx, err := r.varint()
		if err != nil {
			return err
		}
		largest, err := r.varint()
		if err != nil || len(body)-r.i < pathAckBits/8 {
			return errors.New("bad pathack")
		}
		bm := body[r.i : r.i+pathAckBits/8]
		s.mu.Lock()
		for _, q := range s.paths {
			if q.index == idx && q.state != stateDead {
				s.onPathAckLocked(q, largest, bm, time.Now())
			}
		}
		s.mu.Unlock()
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
		var pathAcks [][]byte
		for _, p := range s.paths {
			if p.rxDirty {
				p.rxDirty = false
				pathAcks = append(pathAcks, append([]byte{dgCtrl}, p.pathAckFrameLocked()...))
			}
		}
		paths := s.ackPathsLocked()
		s.mu.Unlock()
		for _, d := range pathAcks {
			for _, p := range paths {
				p.sendDgram(d)
			}
		}
		// ACKs are unreliable datagrams (no head-of-line blocking behind lost
		// packets), sent redundantly on two paths and refreshed periodically.
		for _, d := range encodeAckDatagrams(entries) {
			for _, p := range paths {
				p.sendDgram(d)
			}
		}
	}
}

const tickInterval = 10 * time.Millisecond

const reAckInterval = 30 * time.Millisecond

func (s *Session) tickLoop() {
	t := time.NewTimer(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-s.ctx.Done():
			return
		}
		if s.tick(time.Now()) {
			t.Reset(tickInterval)
		} else {
			t.Reset(idleTickInterval) // nothing in flight: tick slowly
		}
	}
}

const idleTickInterval = 100 * time.Millisecond

// tick runs periodic maintenance; it returns whether the session is busy.
func (s *Session) tick(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	active := len(s.streams) > 0
	changed := false
	for _, p := range s.paths {
		prevMode := p.ccMode
		p.refreshStatsLocked()
		if debugWindows && p.ccMode != prevMode {
			fmt.Fprintf(os.Stderr, "%s CCMODE %s %s -> %s\n", now.Format("15:04:05.000"), p.name, prevMode, p.ccMode)
		}
		if p.tickLocked(now, active) {
			changed = true
		}
	}
	s.updateBenchedLocked()
	s.sharedTickLocked(now)
	if debugWindows {
		for _, st := range s.streams {
			if len(st.oooRanges) > 0 && now.Sub(st.lastDeliverAt) > holDebugThreshold && now.Sub(st.lastHOLLog) > 100*time.Millisecond {
				st.lastHOLLog = now
				fmt.Fprintf(os.Stderr, "%s HOL st=%d missing off=%d (next ooo off=%d) for %v\n", now.Format("15:04:05.000"), st.id, st.recvOff, st.oooRanges[0].start, now.Sub(st.lastDeliverAt).Round(time.Millisecond))
			}
		}
	}
	if now.Sub(s.lastReAck) >= reAckInterval {
		// Refresh ACKs of streams that recently received data: ACK datagrams
		// may be lost, and the sender's reinjection relies on timely SACKs.
		s.lastReAck = now
		for _, st := range s.streams {
			if len(st.ooo) > 0 || now.Sub(st.lastRecvAt) < 500*time.Millisecond {
				s.markAckLocked(st)
			}
		}
		for _, p := range s.paths {
			if now.Sub(p.rxAt) < 500*time.Millisecond {
				p.rxDirty = true
			}
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
		if debugWindows {
			for _, st := range s.streams {
				fmt.Fprintf(os.Stderr, "WIN dialer=%v st=%d recvWin=%dMB inflightSpan=%dMB peerWin-acked=%dMB unsent=%dMB ooo=%d ready=%dKB\n",
					s.dialer, st.id, st.recvWindow>>20, (st.writeOff-st.ackedOff)>>20, (st.peerWindow-st.ackedOff)>>20, st.unsent>>20, len(st.ooo), st.readyBytes>>10)
			}
		}
		for _, st := range s.streams {
			if now.Sub(st.lastActive) > s.opt.IdleTimeout {
				st.resetLocked(ResetIdleTimeout, true)
			} else if s.acceptFn != nil && st.recvOff == 0 && now.Sub(st.created) > 30*time.Second {
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
		args := []any{"streams", len(s.streams), "backlog", s.backlog, "paths", s.statsStringLocked()}
		if s.oversizeDrops > 0 {
			args = append(args, "udp_too_large", s.oversizeDrops)
		}
		s.log.Info("stats", args...)
	}
	return len(s.streams) > 0 || len(s.reinjectQ) > 0 || len(s.unacked) > 0
}

// UDP relaying over QUIC DATAGRAMs. A datagram goes out unreliably on the best
// path that can carry it in one QUIC packet. One that no path can carry is
// dropped, exactly as an IP network with that MTU would drop it: senders that
// discover the path MTU themselves (e.g. QUIC inside the tunnel) settle at a
// size that fits. While a path's MTU discovery runs (from 1200-byte packets),
// the limit may still grow. Relaying UDP over streams instead (the proxy's
// reliable UDP mode) has no size limit.

// DatagramTooLargeError is returned by SendDatagram for a dropped datagram.
type DatagramTooLargeError struct {
	Size int // payload bytes
	Max  int // largest payload the paths currently carry
}

func (e *DatagramTooLargeError) Error() string {
	return fmt.Sprintf("UDP datagram with %d-byte payload exceeds the tunnel's limit of %d bytes", e.Size, e.Max)
}

// SendDatagram sends b as an unreliable datagram (see above). The last
// len(b)-hdrLen bytes of b are the relayed UDP payload.
func (s *Session) SendDatagram(b []byte, hdrLen int) error {
	s.mu.Lock()
	paths := s.bestPathsLocked(len(s.paths))
	s.mu.Unlock()
	if len(paths) == 0 {
		return errors.New("no path available")
	}
	msg := append([]byte{dgUDP}, b...)
	limit := -1 // largest payload some path carries
	var lastErr error
	for _, p := range paths {
		err := p.conn.SendDatagram(msg)
		if err == nil {
			return nil
		}
		var tl *quic.DatagramTooLargeError
		if !errors.As(err, &tl) {
			lastErr = err // e.g. the path is closing
			continue
		}
		limit = max(limit, int(tl.MaxDatagramPayloadSize)-1-hdrLen)
	}
	if limit < 0 {
		return lastErr
	}
	size := len(b) - hdrLen
	s.noteOversize(size, max(limit, 0), time.Now())
	return &DatagramTooLargeError{Size: size, Max: max(limit, 0)}
}

// noteOversize counts a dropped datagram and says so at most every 30 s.
func (s *Session) noteOversize(size, limit int, now time.Time) {
	s.mu.Lock()
	s.oversizeDrops++
	n := s.oversizeDrops
	warn := now.Sub(s.oversizeWarnAt) >= 30*time.Second
	if warn {
		s.oversizeWarnAt = now
	}
	s.mu.Unlock()
	if warn {
		s.log.Warn("UDP datagram too large for the tunnel, dropped (as a network with a smaller MTU would); "+
			"lower the UDP client's MTU, or relay UDP over streams (--reliable-udp)", "payload", size, "max_payload", limit, "dropped_total", n)
	}
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
	Index     int // the dialer's configured path index
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
	Loss      float64 // estimated packet loss rate
	// StaleResets: BBR bandwidth estimates reset after a rate drop
	StaleResets uint64
	// ReinjectBytes / DupBytes: chunk bytes sent again on this path, as
	// reinjections (rescues of late or lost data, insurance copies) and
	// opportunistic or eager duplicates. Included in SentBytes.
	ReinjectBytes, DupBytes uint64
	// SharedCap: the path shares a bottleneck with other paths of the
	// session and its rate is capped at this share of it (bytes/s); 0: not
	// capped.
	SharedCap uint64
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
		fmt.Fprintf(&b, "%s[%s rtt=%v bw=%.1fMbit/s tx=%dMB rx=%dMB lost=%dKB", st.Name, st.State,
			st.SRTT.Round(time.Millisecond), float64(st.Bandwidth)*8/1e6, st.SentBytes>>20, st.RecvBytes>>20, st.LostBytes>>10)
		if st.StaleResets > 0 {
			fmt.Fprintf(&b, " rate_drops=%d", st.StaleResets)
		}
		if g := p.sbs.group; g != nil {
			fmt.Fprintf(&b, " shared=%.1fMbit/s", g.c*8/1e6)
			if st.SharedCap > 0 {
				fmt.Fprintf(&b, " cap=%.1fMbit/s", float64(st.SharedCap)*8/1e6)
			}
		}
		b.WriteString("]")
	}
	return b.String()
}
