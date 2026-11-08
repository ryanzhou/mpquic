package mp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"sync"
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
	sess   *Session
	name   string
	index  uint64
	id     int
	conn   *quic.Conn
	meter  *cc.Meter
	ctrl   *quic.Stream
	ctrlR  *bufio.Reader
	ctrlQ  chan []byte
	dgramQ chan []byte // control datagrams (unreliable: PING/PONG/ACK)
	wake   chan struct{}

	// guarded by sess.mu
	state        pathState
	suspectSince time.Time
	upSince      time.Time
	srtt, rttvar time.Duration
	minRTT       time.Duration
	latestRTT    time.Duration
	bw, cwnd     uint64
	pacing       uint64
	ccMode       string
	staleResets  uint64 // BBR stale-estimate resets (rate drops)
	// measured send rate (fallback when the controller exposes no estimate)
	measRate  float64
	measAt    time.Time
	measBytes uint64
	lost      uint64
	// loss estimates: QUIC packet loss of what we send (EWMA), and round-trip
	// loss of ping datagrams
	lossRate    float64
	pingLoss    float64
	lossPktSent uint64
	lossPktLost uint64
	lossAt      time.Time
	// send time of the latest chunk sent only on this path that has been
	// acknowledged (RACK-style loss detection)
	ackedSentMax time.Time
	// largest chunk payload that fits in one data datagram
	dgramChunk int
	// datagram data: sender-side record of outstanding datagrams by dseq,
	// dgSent[dgHead:] (consecutive dseqs; see dropDgSent)
	dgNext uint64
	dgSent []dgSent
	dgHead int
	// receiver side: which dseqs arrived (bit i of rxBits = dseq rxLargest-i)
	rxLargest uint64
	rxBits    [pathAckBits/64 + 1]uint64
	rxDirty   bool
	rxAt      time.Time
	busy      bool
	busyChunk *chunk    // chunk the send loop is currently writing
	burstAt   time.Time // first multi-packet chunk sent (see rate)
	// late: recent peak queueing delay beyond what SRTT includes (seconds,
	// see updateLateLocked)
	queuePeak float64
	late      float64
	lateAt    time.Time
	burstSeen bool // ... and acknowledged: bw reflects the bottleneck
	benched   bool // in ProbeRTT and another path can stand in (see usable)
	busySince time.Time
	busyLen   int
	pings     []pingRec
	pingSeq   uint64
	lastPing  time.Time
	lastPong  time.Time
	lastRecv  time.Time
	closeIDs  []uint64

	sentBytes atomic.Uint64
	recvBytes atomic.Uint64
	// chunk bytes scheduled as copies of data already sent (sess.mu held):
	// reinjections/rescues and opportunistic duplicates
	reinjBytes, dupBytes uint64
	// shared-bottleneck detection and coupling (see shared.go)
	sbs sbPathState

	// owned by the send loop
	sendStreams map[uint64]*quic.SendStream
	streamUses  map[uint64]int
}

func newPath(s *Session, name string, index uint64, conn *quic.Conn, meter *cc.Meter, ctrl *quic.Stream, ctrlR *bufio.Reader) *Path {
	now := time.Now()
	p := &Path{
		sess: s, name: name, index: index, conn: conn, meter: meter, ctrl: ctrl, ctrlR: ctrlR,
		ctrlQ:       make(chan []byte, 1024),
		dgramQ:      make(chan []byte, 256),
		dgramChunk:  dgramChunkDefault,
		streamUses:  make(map[uint64]int),
		wake:        make(chan struct{}, 1),
		sendStreams: make(map[uint64]*quic.SendStream),
		lastPong:    now,
		lastRecv:    now,
	}
	if meter != nil {
		meter.SetProbeGate(&s.probeGate)
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
	go p.recvDatagrams(ctx)
	go p.sendDatagrams(ctx)
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

// sendDgram queues an unreliable control datagram (dropped if the queue is
// full: everything sent this way is periodic or redundant).
func (p *Path) sendDgram(b []byte) {
	select {
	case p.dgramQ <- b:
	default:
	}
}

func (p *Path) sendDatagrams(ctx context.Context) {
	for {
		select {
		case b := <-p.dgramQ:
			if err := p.conn.SendDatagram(b); err != nil {
				// e.g. too large for the current packet size: fall back to
				// the (reliable) control stream.
				if len(b) > 0 && b[0] == dgCtrl {
					p.sendCtrl(b[1:])
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (p *Path) recvDatagrams(ctx context.Context) {
	for {
		b, err := p.conn.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		if len(b) == 0 {
			continue
		}
		switch b[0] {
		case dgUDP:
			p.sess.deliverDatagram(b[1:])
		case dgData:
			r := &byteReader{b: b, i: 1}
			dseq, err0 := r.varint()
			id, err1 := r.varint()
			off, err2 := r.varint()
			lf, err3 := r.varint()
			if err0 != nil || err1 != nil || err2 != nil || err3 != nil || uint64(len(b)-r.i) != lf>>1 {
				continue
			}
			p.recvBytes.Add(lf >> 1)
			p.sess.onDgramChunk(p, dseq, id, off, b[r.i:], lf&1 == 1)
		case dgCtrl:
			r := bufio.NewReader(bytes.NewReader(b[1:]))
			for {
				typ, body, err := readFrame(r)
				if err != nil {
					break
				}
				if p.sess.onCtrl(p, typ, body) != nil {
					break
				}
			}
		}
	}
}

var uniReaders = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 32<<10) }}

const maxChunkWire = 1 << 20

func (p *Path) readUni(rs *quic.ReceiveStream) {
	br := uniReaders.Get().(*bufio.Reader)
	br.Reset(rs)
	defer func() { br.Reset(nil); uniReaders.Put(br) }()
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
	var hdr, dbuf []byte
	for {
		c, dseq, perStream, ids, err := p.sess.nextChunk(p)
		dgram := dseq >= 0
		for _, id := range ids {
			if ss := p.sendStreams[id]; ss != nil {
				ss.Close()
				delete(p.sendStreams, id)
				delete(p.streamUses, id)
			}
		}
		if err != nil {
			return
		}
		if c == nil {
			continue
		}
		id := c.st.id
		if dgram {
			lf := uint64(len(c.data)) << 1
			if c.fin {
				lf |= 1
			}
			// SendDatagram copies the payload: the buffer is reused.
			b := append(dbuf[:0], dgData)
			b = quicvarint.Append(b, uint64(dseq))
			b = quicvarint.Append(b, id)
			b = quicvarint.Append(b, c.off)
			b = quicvarint.Append(b, lf)
			b = append(b, c.data...)
			dbuf = b
			if err := p.conn.SendDatagram(b); err != nil {
				var tl *quic.DatagramTooLargeError
				if errors.As(err, &tl) {
					p.sess.datagramTooLarge(p, int(tl.MaxDatagramPayloadSize))
				}
				p.sess.sendFailed(c, p)
				if p.conn.Context().Err() != nil {
					return
				}
				continue
			}
			p.sentBytes.Add(uint64(len(c.data)))
			continue
		}
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
			delete(p.streamUses, id)
			ss.CancelWrite(0)
			if p.conn.Context().Err() != nil {
				return
			}
			continue
		}
		p.sentBytes.Add(uint64(len(c.data)))
		// Rotate QUIC streams so that one lost packet only blocks the chunks
		// that share its stream: every chunk gets its own stream on lossy
		// paths, and streams are recycled regularly on clean ones.
		p.streamUses[id]++
		if p.streamUses[id] >= perStream {
			ss.Close()
			delete(p.sendStreams, id)
			delete(p.streamUses, id)
		}
	}
}

// ---- estimation helpers (sess.mu held) ----

func (p *Path) refreshStatsLocked() {
	st := p.conn.ConnectionStats()
	p.srtt = st.SmoothedRTT
	p.rttvar = st.MeanDeviation
	p.minRTT = st.MinRTT
	p.latestRTT = st.LatestRTT
	p.lost = st.BytesLost
	sent, lost := st.PacketsSent, st.PacketsLost
	if p.meter != nil {
		// quic-go counts losses only in its built-in controller
		m := p.meter.Snapshot()
		sent, lost = m.PacketsSent, m.PacketsLost
	}
	if now := time.Now(); now.Sub(p.lossAt) >= 100*time.Millisecond {
		ds := int64(sent) - int64(p.lossPktSent)
		dl := int64(lost) - int64(p.lossPktLost)
		if ds >= 20 && !p.lossAt.IsZero() {
			inst := min(max(float64(dl)/float64(ds), 0), 1)
			p.lossRate = 0.8*p.lossRate + 0.2*inst
			p.lossPktSent, p.lossPktLost, p.lossAt = sent, lost, now
		} else if p.lossAt.IsZero() {
			p.lossPktSent, p.lossPktLost, p.lossAt = sent, lost, now
		}
	}
	if now := time.Now(); now.Sub(p.measAt) >= 100*time.Millisecond {
		b := p.sentBytes.Load()
		if !p.measAt.IsZero() {
			inst := float64(b-p.measBytes) / now.Sub(p.measAt).Seconds()
			p.measRate = max(inst, p.measRate*0.95)
		}
		p.measAt, p.measBytes = now, b
	}
	if !p.burstSeen && !p.burstAt.IsZero() && time.Since(p.burstAt) > 2*p.srttOrDefault() {
		p.burstSeen = true
	}
	if p.meter != nil {
		m := p.meter.Snapshot()
		p.bw = m.Bandwidth
		p.cwnd = m.Cwnd
		p.pacing = m.Pacing
		p.ccMode = m.Mode
		p.staleResets = m.StaleResets
	}
}

func (p *Path) srttOrDefault() time.Duration {
	if p.srtt <= 0 {
		return 200 * time.Millisecond
	}
	return p.srtt
}

// schedRTT is the RTT used for scheduling: the smoothed RTT, but reacting
// immediately when the latest sample shows that queues have drained.
func (p *Path) schedRTT() time.Duration {
	r := p.srttOrDefault()
	if l := max(p.latestRTT, p.minRTT); l > 0 && l < r {
		r = l
	}
	return r
}

// fwdDelay converts an RTT into the one-way delay of data we send: queueing
// builds in the data direction only (the ACK direction is idle), so the
// forward delay is the RTT minus the base one-way delay of the return trip.
func (p *Path) fwdDelay(rtt time.Duration) float64 {
	if p.minRTT > 0 && p.minRTT < rtt {
		return (rtt - p.minRTT/2).Seconds()
	}
	return rtt.Seconds() / 2
}

// owd is the estimated one-way delay of data sent now, in seconds.
func (p *Path) owd() float64 { return p.fwdDelay(p.schedRTT()) }

// owdTail is a pessimistic one-way delay (+ RTTVAR): decisions whose failure
// would stall the stream are based on it, so that jittery paths are not
// mistaken for fast ones.
func (p *Path) owdTail() float64 { return p.owd() + p.rttvar.Seconds() }

// arrivalBound is a generous bound (seconds from now) on when n bytes sent now
// arrive: smoothed RTT plus 4 RTTVAR (halved), transmission, a stalled
// writer's penalty and the expected loss-recovery delay.
func (p *Path) arrivalBound(n int, now time.Time) float64 {
	return p.fwdDelay(p.srttOrDefault()) + 2*p.rttvar.Seconds() + p.penalty(now) + float64(n)/p.rate() + p.lossDelay(n)
}

// deadlineBound is the arrival bound (seconds after sending) after which an
// unacknowledged chunk that the stream needs is rescued on another path. On
// a path with some loss a stream send gets one QUIC recovery round on top:
// QUIC retransmits a lost packet on the path itself, typically in time, and
// rescuing every such chunk elsewhere as well would spend the other paths'
// capacity on duplicates.
func (p *Path) deadlineBound(n int, dgram bool, now time.Time) float64 {
	b := p.arrivalBound(n, now)
	if !dgram && p.loss() > 0.003 {
		b += 1.125*p.srttOrDefault().Seconds() + p.owd()
	}
	return b
}

// rescueCost ranks paths for resending a chunk that is already late: the
// expected delivery time plus a penalty for the risk of losing it again.
func (p *Path) rescueCost(n int, now time.Time) float64 {
	return p.est(n, now) + p.loss()*2*p.srttOrDefault().Seconds()
}

// usable reports whether new data may be scheduled on the path (and whether
// it counts as capacity in scheduling decisions): not during BBR ProbeRTT,
// when its window is cut to 4 packets for ~200 ms.
func (p *Path) usable() bool { return p.state == stateActive && !p.benched }

// benchedLocked: p is in BBR ProbeRTT (window cut to 4 packets for ~200 ms)
// and another path can carry new data meanwhile. With no alternative, a
// probing path still sends.
func (p *Path) benchedLocked() bool {
	if p.state != stateActive || !p.probingRTT() {
		return false
	}
	for _, q := range p.sess.paths {
		if q != p && q.state == stateActive && !q.probingRTT() {
			return true
		}
	}
	return false
}

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
	// The cwnd floor covers BBR's estimate being app-limited (paths left
	// idle); once out of STARTUP with a real estimate, trust it (on thin
	// paths the floor would overestimate badly). A path that only ever
	// carried small messages stays in STARTUP indefinitely, with its initial
	// window: once it has carried a burst (so that BBR's estimate reflects
	// the bottleneck rather than ping traffic), the floor is capped at twice
	// the measured rate (BBR's STARTUP growth per round), so a thin path is
	// not mistaken for a fast one just because it was never loaded.
	if c := float64(p.cwnd) / (2 * mr.Seconds()); c > r && (p.bw == 0 || p.ccMode == "startup" || p.ccMode == "") {
		if p.bw > 0 && p.burstSeen {
			c = min(c, 2*r)
		}
		r = max(r, c)
	}
	if p.meter == nil && p.bw == 0 {
		// controller without a bandwidth estimate (cubic): measured rate
		r = max(p.measRate, float64(32*1200)/mr.Seconds())
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

// loss is the estimated packet loss probability of data sent on this path.
func (p *Path) loss() float64 {
	l := max(p.lossRate, p.pingLoss/2) // a ping round trip crosses the path twice
	return min(l, 0.9)
}

// lossDelay is the expected extra delivery delay (seconds) of an n-byte chunk
// caused by packet loss: the probability that any of its packets is lost
// times QUIC's recovery time (loss detection at ~9/8 RTT plus the
// retransmission's one-way trip, repeated while retransmissions are lost).
func (p *Path) lossDelay(n int) float64 {
	l := p.loss()
	if l < 0.001 {
		return 0
	}
	pkts := float64(n/1200 + 1)
	hit := 1 - math.Pow(1-l, pkts)
	return hit * 1.125 * p.srttOrDefault().Seconds() / (1 - l)
}

// reorderWindow is how much later a chunk must have been sent on this path
// than an unacknowledged one, and been acknowledged, for the earlier one to be
// declared lost (tolerates jitter-induced reordering).
func (p *Path) reorderWindow() time.Duration {
	return max(p.srttOrDefault()/4, 2*p.rttvar, 5*time.Millisecond)
}

// lossy reports whether the path loses enough packets to change strategy.
func (p *Path) lossy() bool { return p.loss() > 0.01 }

// dgramMode: on very lossy paths new data is sent as unreliable DATAGRAMs, one
// packet per chunk. There is no head-of-line blocking at all, the bounded
// datagram queue provides backpressure, and a lost chunk is not retransmitted
// on this path by QUIC but detected by the session (RACK / timeout) and
// reinjected on the best available path.
// dgramChunkDefault leaves room in the packet for a QUIC ACK frame: quic-go
// silently discards a DATAGRAM that does not fit next to the packet's ACK
// frame, and on lossy paths ACK frames carry many ranges.
const dgramChunkDefault = 960

func (p *Path) dgramMode() bool { return p.loss() >= dgramLossThreshold }

var dgramLossThreshold = func() float64 {
	if v, err := strconv.ParseFloat(os.Getenv("MPQUIC_DGRAM_LOSS"), 64); err == nil {
		return v
	}
	return 0.08
}()

// canCarry: a very lossy path should not carry large reinjected/duplicated
// chunks (they would go through a QUIC stream and suffer its loss recovery).
func (p *Path) canCarry(c *chunk) bool {
	return !p.dgramMode() || len(c.data) <= 4*p.dgramChunk
}

// maxChunk is the largest chunk this path should carry: small enough that the
// chance of losing any of its packets stays <= 25% (one packet at 20% loss,
// two at 10%, five at 5%; unlimited below ~1%), and one datagram in dgramMode.
func (p *Path) maxChunk() int {
	if p.dgramMode() {
		return p.dgramChunk
	}
	l := p.loss()
	if l < 0.002 {
		return 1 << 30
	}
	pkts := math.Floor(math.Log(0.75) / math.Log(1-l))
	return max(int(pkts), 1) * 1150
}

// chunksPerStream is how many chunks share one QUIC stream before rotation.
func (p *Path) chunksPerStream() int {
	if p.loss() > 0.003 {
		return 1
	}
	return 64
}

// probingRTT: BBR periodically cuts its window to 4 packets for ~200 ms to
// re-measure the minimum RTT; no new data should be handed to it meanwhile.
func (p *Path) probingRTT() bool { return p.ccMode == "probe_rtt" }

// est is the estimated delivery time (seconds from now) of n bytes sent now,
// including the expected loss-recovery delay and recent lateness.
func (p *Path) est(n int, now time.Time) float64 {
	return p.owdTail() + p.penalty(now) + float64(n)/p.rate() + p.lossDelay(n) + p.late
}

// lateHalfLife: how quickly a path's record of queueing spikes fades.
const lateHalfLife = 5 * time.Second

// updateLateLocked tracks the path's recent peak queueing delay (latest RTT
// above the minimum, a decaying maximum) and sets late to how far it exceeds
// the queueing already included in SRTT. A path whose delay spikes, e.g. a
// relay whose bandwidth collapses now and then, thereby counts as slower
// than its current RTT suggests: it is not trusted with the stream's front,
// and data it takes is placed further ahead.
func (p *Path) updateLateLocked(now time.Time) {
	if !p.lateAt.IsZero() {
		p.queuePeak *= math.Exp2(-now.Sub(p.lateAt).Seconds() / lateHalfLife.Seconds())
	}
	p.lateAt = now
	if p.minRTT > 0 && p.latestRTT > p.minRTT {
		p.queuePeak = max(p.queuePeak, min((p.latestRTT-p.minRTT).Seconds(), 5))
	}
	p.late = 0
	if p.minRTT > 0 {
		p.late = max(p.queuePeak-max(p.srtt-p.minRTT, 0).Seconds(), 0)
	}
}

// pingInterval is how often liveness pings are sent.
func pingInterval(active bool) time.Duration {
	if active {
		return 100 * time.Millisecond
	}
	return time.Second
}

// suspectThreshold is how long no ping may be answered before the path is
// suspect. Pings are unreliable datagrams, so the threshold covers enough
// consecutive ping intervals that, given the measured ping loss, a healthy
// path is falsely suspected with probability < 1e-4.
func (p *Path) suspectThreshold() time.Duration {
	t := 3*p.srttOrDefault() + 4*p.rttvar
	if t < p.sess.opt.SuspectTimeout {
		t = p.sess.opt.SuspectTimeout
	}
	pl := min(max(p.pingLoss, 0.02), 0.8)
	k := math.Ceil(math.Log(1e-4) / math.Log(pl))
	if c := time.Duration(k)*pingInterval(true) + p.srttOrDefault(); c > t {
		t = c
	}
	return t
}

func (p *Path) onPongLocked(seq uint64, now time.Time) {
	p.lastPong = now
	p.lastRecv = now
	i := 0
	skipped := 0
	for i < len(p.pings) && p.pings[i].seq <= seq {
		if p.pings[i].seq < seq {
			skipped++ // earlier ping never answered: lost (or reordered)
		}
		i++
	}
	if i > 0 {
		p.pingLoss = 0.9*p.pingLoss + 0.1*float64(skipped)/float64(skipped+1)
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
	p.updateLateLocked(now)
	if now.Sub(p.lastPing) >= pingInterval(active) {
		p.pingSeq++
		p.lastPing = now
		if len(p.pings) < 256 {
			p.pings = append(p.pings, pingRec{p.pingSeq, now})
		}
		p.sendDgram(append([]byte{dgCtrl}, encodeU64s(frPing, p.pingSeq)...))
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

// outstandingDgrams returns the records of datagrams not yet resolved
// (acknowledged or declared lost) at the front, in dseq order.
func (p *Path) outstandingDgrams() []dgSent { return p.dgSent[p.dgHead:] }

// dropDgSent drops the n oldest outstanding records. The backing array is
// reused: it is compacted once the dropped prefix is as long as the rest, so
// that steady-state appends do not reallocate it.
func (p *Path) dropDgSent(n int) {
	if n <= 0 {
		return
	}
	clear(p.dgSent[p.dgHead : p.dgHead+n]) // release the chunks
	p.dgHead += n
	switch {
	case p.dgHead == len(p.dgSent):
		p.dgSent, p.dgHead = p.dgSent[:0], 0
	case p.dgHead >= 256 && 2*p.dgHead >= len(p.dgSent):
		k := copy(p.dgSent, p.dgSent[p.dgHead:])
		clear(p.dgSent[k:])
		p.dgSent, p.dgHead = p.dgSent[:k], 0
	}
}

type dgSent struct {
	seq      uint64
	c        *chunk
	at       time.Time
	resolved bool
}

// rxMarkLocked records receipt of datagram dseq (receiver side).
func (p *Path) rxMarkLocked(seq uint64, now time.Time) {
	p.rxDirty, p.rxAt = true, now
	if seq > p.rxLargest || p.rxLargest == 0 && p.rxBits[0] == 0 {
		shift := seq - p.rxLargest
		if p.rxLargest == 0 && p.rxBits[0] == 0 {
			shift = 0
		}
		p.rxShift(shift)
		p.rxLargest = seq
		p.rxBits[0] |= 1
		return
	}
	d := p.rxLargest - seq
	if d < uint64(len(p.rxBits)*64) {
		p.rxBits[d/64] |= 1 << (d % 64)
	}
}

func (p *Path) rxShift(n uint64) {
	if n == 0 {
		return
	}
	if n >= uint64(len(p.rxBits)*64) {
		p.rxBits = [len(p.rxBits)]uint64{}
		return
	}
	w, b := int(n/64), n%64
	for i := len(p.rxBits) - 1; i >= 0; i-- {
		var v uint64
		if i-w >= 0 {
			v = p.rxBits[i-w] << b
			if b > 0 && i-w-1 >= 0 {
				v |= p.rxBits[i-w-1] >> (64 - b)
			}
		}
		p.rxBits[i] = v
	}
}

// pathAckFrameLocked encodes this path's datagram receipt state.
func (p *Path) pathAckFrameLocked() []byte {
	body := quicvarint.Append(nil, p.index)
	body = quicvarint.Append(body, p.rxLargest)
	var bm [pathAckBits / 8]byte
	for i := 0; i < pathAckBits; i++ { // bit i: dseq largest-1-i
		d := uint64(i + 1)
		if p.rxBits[d/64]&(1<<(d%64)) != 0 {
			bm[i/8] |= 1 << (i % 8)
		}
	}
	body = append(body, bm[:]...)
	return appendFrame(nil, frPathAck, body)
}

func (p *Path) statLocked() PathStat {
	state := p.state.String()
	return PathStat{
		Name: p.name, Index: int(p.index), State: state, SRTT: p.srtt, MinRTT: p.minRTT,
		Bandwidth: p.bw, Cwnd: p.cwnd, Pacing: p.pacing, CCMode: p.ccMode,
		SentBytes: p.sentBytes.Load(), RecvBytes: p.recvBytes.Load(), LostBytes: p.lost, Loss: p.loss(),
		StaleResets: p.staleResets, ReinjectBytes: p.reinjBytes, DupBytes: p.dupBytes,
		SharedCap: p.sbs.cap,
	}
}
