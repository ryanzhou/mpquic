package mp

import (
	"bufio"
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/quicvarint"

	"mpquic/internal/cc"
)

type pathState int

const (
	stateActive pathState = iota
	stateSuspect
	stateDead
)

func (s pathState) String() string {
	switch s {
	case stateActive:
		return "active"
	case stateSuspect:
		return "suspect"
	}
	return "dead"
}

type pingRec struct {
	seq uint64
	at  time.Time
}

// Path is one QUIC connection (possibly via a relay) belonging to a session.
type Path struct {
	sess  *Session
	name  string
	index uint64
	id    int
	conn  *quic.Conn
	meter *cc.Meter
	ctrl  *quic.Stream
	ctrlR *bufio.Reader
	ctrlQ chan []byte
	wake  chan struct{}

	// guarded by sess.mu
	state        pathState
	suspectSince time.Time
	upSince      time.Time
	srtt, rttvar time.Duration
	minRTT       time.Duration
	bw, cwnd     uint64
	lost         uint64
	busy         bool
	busySince    time.Time
	busyLen      int
	pings        []pingRec
	pingSeq      uint64
	lastPing     time.Time
	lastPong     time.Time
	lastRecv     time.Time
	closeIDs     []uint64

	sentBytes atomic.Uint64
	recvBytes atomic.Uint64

	// owned by the send loop
	sendStreams map[uint64]*quic.SendStream
}

func newPath(s *Session, name string, index uint64, conn *quic.Conn, meter *cc.Meter, ctrl *quic.Stream, ctrlR *bufio.Reader) *Path {
	now := time.Now()
	p := &Path{
		sess: s, name: name, index: index, conn: conn, meter: meter, ctrl: ctrl, ctrlR: ctrlR,
		ctrlQ:       make(chan []byte, 1024),
		wake:        make(chan struct{}, 1),
		sendStreams: make(map[uint64]*quic.SendStream),
		lastPong:    now,
		lastRecv:    now,
	}
	p.refreshStatsLocked()
	return p
}

// run serves the path until its connection ends.
func (p *Path) run() error {
	ctx := p.conn.Context()
	go p.ctrlWriter(ctx)
	go p.ctrlReader()
	go p.acceptUni(ctx)
	go p.sendLoop()
	<-ctx.Done()
	err := context.Cause(ctx)
	p.sess.removePath(p, err)
	return err
}

func (p *Path) sendCtrl(b []byte) {
	select {
	case p.ctrlQ <- b:
	default:
	}
}

func (p *Path) ctrlWriter(ctx context.Context) {
	buf := make([]byte, 0, 64<<10)
	for {
		select {
		case b := <-p.ctrlQ:
			buf = append(buf[:0], b...)
		drain:
			for len(buf) < 32<<10 {
				select {
				case b := <-p.ctrlQ:
					buf = append(buf, b...)
				default:
					break drain
				}
			}
			if _, err := p.ctrl.Write(buf); err != nil {
				p.conn.CloseWithError(errCodeShutdown, "control write: "+err.Error())
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (p *Path) ctrlReader() {
	for {
		typ, body, err := readFrame(p.ctrlR)
		if err != nil {
			p.conn.CloseWithError(errCodeShutdown, "control read: "+err.Error())
			return
		}
		if err := p.sess.onCtrl(p, typ, body); err != nil {
			p.conn.CloseWithError(errCodeShutdown, "control frame: "+err.Error())
			return
		}
	}
}

func (p *Path) acceptUni(ctx context.Context) {
	for {
		rs, err := p.conn.AcceptUniStream(ctx)
		if err != nil {
			return
		}
		go p.readUni(rs)
	}
}

const maxChunkWire = 1 << 20

func (p *Path) readUni(rs *quic.ReceiveStream) {
	br := bufio.NewReaderSize(rs, 64<<10)
	id, err := quicvarint.Read(br)
	if err != nil {
		return
	}
	for {
		off, err := quicvarint.Read(br)
		if err != nil {
			return
		}
		lf, err := quicvarint.Read(br)
		if err != nil {
			return
		}
		n := lf >> 1
		if n > maxChunkWire {
			p.conn.CloseWithError(errCodeShutdown, "chunk too large")
			return
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(br, data); err != nil {
			return
		}
		p.recvBytes.Add(n)
		p.sess.onChunk(p, id, off, data, lf&1 == 1)
	}
}

func (p *Path) sendLoop() {
	defer func() {
		for _, ss := range p.sendStreams {
			ss.CancelWrite(0)
		}
	}()
	var hdr []byte
	for {
		c, ids, err := p.sess.nextChunk(p)
		for _, id := range ids {
			if ss := p.sendStreams[id]; ss != nil {
				ss.Close()
				delete(p.sendStreams, id)
			}
		}
		if err != nil {
			return
		}
		if c == nil {
			continue
		}
		id := c.st.id
		ss := p.sendStreams[id]
		if ss == nil {
			ss, err = p.conn.OpenUniStreamSync(p.conn.Context())
			if err != nil {
				return
			}
			p.sendStreams[id] = ss
			hdr = quicvarint.Append(hdr[:0], id)
			if _, err := ss.Write(hdr); err != nil {
				return
			}
		}
		lf := uint64(len(c.data)) << 1
		if c.fin {
			lf |= 1
		}
		hdr = quicvarint.Append(hdr[:0], c.off)
		hdr = quicvarint.Append(hdr, lf)
		_, err = ss.Write(hdr)
		if err == nil && len(c.data) > 0 {
			_, err = ss.Write(c.data)
		}
		if err != nil {
			// The chunk is recovered by reinjection; drop the broken stream.
			delete(p.sendStreams, id)
			ss.CancelWrite(0)
			if p.conn.Context().Err() != nil {
				return
			}
			continue
		}
		p.sentBytes.Add(uint64(len(c.data)))
	}
}

// ---- estimation helpers (sess.mu held) ----

func (p *Path) refreshStatsLocked() {
	st := p.conn.ConnectionStats()
	p.srtt = st.SmoothedRTT
	p.rttvar = st.MeanDeviation
	p.minRTT = st.MinRTT
	p.lost = st.BytesLost
	if p.meter != nil {
		m := p.meter.Snapshot()
		p.bw = m.Bandwidth
		p.cwnd = m.Cwnd
	}
}

func (p *Path) srttOrDefault() time.Duration {
	if p.srtt <= 0 {
		return 200 * time.Millisecond
	}
	return p.srtt
}

// owd is the estimated one-way delay in seconds.
func (p *Path) owd() float64 { return p.srttOrDefault().Seconds() / 2 }

// rate is the estimated sending rate in bytes/s. BBR's bottleneck estimate is
// unreliable while the path is application-limited (e.g. it only carried
// ACKs), so it is floored by cwnd/(2*minRTT), which equals the bottleneck
// rate in BBR steady state (cwnd = 2*BDP) and is optimistic during startup.
func (p *Path) rate() float64 {
	r := float64(p.bw)
	mr := p.minRTT
	if mr <= 0 {
		mr = p.srttOrDefault()
	}
	if c := float64(p.cwnd) / (2 * mr.Seconds()); c > r {
		r = c
	}
	if r < 16<<10 {
		r = 16 << 10
	}
	return r
}

// penalty is extra delay (seconds) if the path's writer seems stuck.
func (p *Path) penalty(now time.Time) float64 {
	if !p.busy {
		return 0
	}
	el := now.Sub(p.busySince).Seconds()
	exp := float64(p.busyLen) / p.rate()
	if el > 2*exp+0.005 {
		return el - exp
	}
	return 0
}

// est is the estimated delivery time (seconds from now) of n bytes sent now.
func (p *Path) est(n int, now time.Time) float64 {
	return p.owd() + p.penalty(now) + float64(n)/p.rate()
}

func (p *Path) suspectThreshold() time.Duration {
	t := 3*p.srttOrDefault() + 4*p.rttvar
	if t < p.sess.opt.SuspectTimeout {
		t = p.sess.opt.SuspectTimeout
	}
	return t
}

func (p *Path) onPongLocked(seq uint64, now time.Time) {
	p.lastPong = now
	p.lastRecv = now
	i := 0
	for i < len(p.pings) && p.pings[i].seq <= seq {
		i++
	}
	p.pings = p.pings[:copy(p.pings, p.pings[i:])]
	if p.state == stateSuspect && len(p.pings) == 0 || p.state == stateSuspect && now.Sub(p.pings[0].at) < p.suspectThreshold() {
		p.state = stateActive
		p.sess.log.Info("path recovered", "path", p.name, "after", now.Sub(p.suspectSince).Round(time.Millisecond))
		p.sess.wakeAllLocked()
	}
}

// tickLocked sends pings and updates liveness. Returns true if the path just
// became suspect.
func (p *Path) tickLocked(now time.Time, active bool) bool {
	if p.state == stateDead {
		return false
	}
	iv := time.Second
	if active {
		iv = 200 * time.Millisecond
	}
	if now.Sub(p.lastPing) >= iv {
		p.pingSeq++
		p.lastPing = now
		if len(p.pings) < 64 {
			p.pings = append(p.pings, pingRec{p.pingSeq, now})
		}
		p.sendCtrl(encodeU64s(frPing, p.pingSeq))
	}
	if p.state == stateActive && len(p.pings) > 0 && now.Sub(p.pings[0].at) > p.suspectThreshold() {
		p.state = stateSuspect
		p.suspectSince = now
		p.sess.log.Info("path suspect", "path", p.name, "unanswered", now.Sub(p.pings[0].at).Round(time.Millisecond))
		return true
	}
	if p.state == stateSuspect && now.Sub(p.suspectSince) > p.sess.opt.PathDeadTimeout {
		p.sess.log.Info("path dead, closing", "path", p.name)
		go p.conn.CloseWithError(errCodeShutdown, "path dead")
	}
	return false
}

func (p *Path) statLocked() PathStat {
	return PathStat{
		Name: p.name, State: p.state.String(), SRTT: p.srtt, MinRTT: p.minRTT,
		Bandwidth: p.bw, Cwnd: p.cwnd,
		SentBytes: p.sentBytes.Load(), RecvBytes: p.recvBytes.Load(), LostBytes: p.lost,
	}
}
