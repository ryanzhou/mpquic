package bbr

// BBR version 3, after draft-ietf-ccwg-bbr-05 ("BBR Congestion Control").
// The functions below are the draft's, by name and in its order of
// processing; comments say where this implementation differs from its
// pseudocode, and why. The delivery rate samples come from the bandwidth
// sampler next to this file (bandwidth_sampler.go, the QUIC implementation
// of the draft's section 4.1).
//
// What is not the draft's is marked "mpquic": the hooks through which a
// session reaches the controllers of its paths, and the loss tolerance.

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"time"

	"github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"

	"mpquic/internal/cc/common"
)

// The draft's constants (sections 2.7 to 2.13 and 5.6.1).
const (
	lossThresh = 0.02 // BBR.LossThresh: the loss rate that is too high when probing
	beta       = 0.7  // BBR.Beta: the decrease per round trip with loss
	headroom   = 0.15 // BBR.Headroom: left unused of inflight_longterm while cruising

	startupPacingGain = 2.77 // 4*ln(2): the rate doubles every round trip
	drainPacingGain   = 0.5
	defaultCwndGain   = 2.0
	probeUpCwndGain   = 2.25
	probeUpPacingGain = 1.25
	probeDownGain     = 0.9
	pacingMargin      = 0.99 // 100 - BBR.PacingMarginPercent
	startupGrowth     = 1.25 // BBRCheckFullBWReached: bw still grows by this
	fullBwRounds      = 3    // ... or the pipe is full after this many rounds
	startupFullLoss   = 6    // BBRStartupFullLossCnt: loss events in a round

	minRTTFilterLen     = 10 * time.Second
	probeRTTInterval    = 5 * time.Second
	probeRTTDuration    = 200 * time.Millisecond
	probeRTTCwndGain    = 0.5
	extraAckedFilterLen = 10 // rounds

	initialCwndPackets = 32 // quic-go's initial window
	minPipeCwndPackets = 4  // BBR.MinPipeCwnd

	// minPacingRate: the pacer must never see zero (quic-go's send loop
	// would spin), and keepalives must get out.
	minPacingRate = 8 << 10 // bytes/s

	invalidPacketNumber = -1

	debugEnv = "MPQUIC_BBR_DEBUG"
)

type state int

const (
	stStartup state = iota
	stDrain
	stProbeBWDown
	stProbeBWCruise
	stProbeBWRefill
	stProbeBWUp
	stProbeRTT
)

func (s state) String() string {
	return [...]string{"startup", "drain", "probe_bw", "probe_bw", "probe_bw", "probe_bw", "probe_rtt"}[s]
}

func (s state) detail() string {
	return [...]string{"startup", "drain", "down", "cruise", "refill", "up", "probe_rtt"}[s]
}

type ackPhase int

const (
	acksInit ackPhase = iota
	acksRefilling
	acksProbeStarting
	acksProbeFeedback
	acksProbeStopping
)

const (
	infBytes = congestion.ByteCount(math.MaxInt64)
	infTime  = monotime.Time(math.MaxInt64)
)

// rateSample is the draft's RS: what the latest acknowledgement says.
type rateSample struct {
	deliveryRate   Bandwidth            // RS.delivery_rate
	isAppLimited   bool                 // RS.is_app_limited
	rtt            time.Duration        // RS.rtt (infRTT: none)
	newlyAcked     congestion.ByteCount // RS.newly_acked
	newlyLost      congestion.ByteCount // RS.newly_lost
	delivered      congestion.ByteCount // RS.delivered: delivered while the acked packet was in flight
	priorDelivered congestion.ByteCount // RS.prior_delivered: C.delivered when it was sent
	txInFlight     congestion.ByteCount // RS.tx_in_flight: C.inflight when it was sent
	lost           congestion.ByteCount // RS.lost: lost since it was sent
	valid          bool                 // it acknowledged data
}

// Sender is a BBRv3 congestion controller for one QUIC connection.
type Sender struct {
	clock    Clock
	rttStats congestion.RTTStatsProvider
	sampler  *bandwidthSampler
	pacer    *common.Pacer
	mds      congestion.ByteCount // C.SMSS
	debug    bool

	// C.*
	cwnd          congestion.ByteCount
	pacingRate    Bandwidth
	inflight      congestion.ByteCount
	cwndLimited   bool // C.is_cwnd_limited: the window held a packet back since the last ACK
	inRecovery    bool
	recoveryStart congestion.ByteCount // C.delivered when the loss episode began

	// BBR.*
	st            state
	pacingGain    float64
	cwndGain      float64
	roundCount    uint64
	roundStart    bool
	nextRoundDlvd congestion.ByteCount
	idleRestart   bool
	drainStart    uint64

	maxBwFilter [2]Bandwidth // this ProbeBW cycle and the one before
	maxBwAged   bool         // the filter has advanced in this cycle
	maxBw       Bandwidth
	bwShort     Bandwidth // BBR.bw_shortterm (infBandwidth: none)
	bw          Bandwidth
	minRTT      time.Duration
	minRTTStamp monotime.Time

	inflightLong  congestion.ByteCount // BBR.inflight_longterm (infBytes: none)
	inflightShort congestion.ByteCount // BBR.inflight_shortterm
	maxInflight   congestion.ByteCount

	bwLatest       Bandwidth
	inflightLatest congestion.ByteCount
	lossRoundDlvd  congestion.ByteCount
	lossRoundStart bool
	lossInRound    bool
	lossEvents     int // acknowledgements that reported loss in the round (BBRCheckStartupHighLoss)

	fullBwReached bool
	fullBwNow     bool
	fullBw        Bandwidth
	fullBwCount   int

	probeRTTMinDelay  time.Duration
	probeRTTMinStamp  monotime.Time
	probeRTTExpired   bool
	probeRTTDeferred  bool // mpquic: due, but the gate was closed
	probeRTTDoneStamp monotime.Time
	probeRTTRoundDone bool
	priorCwnd         congestion.ByteCount

	cycleStamp         monotime.Time
	bwProbeWait        time.Duration
	roundsSinceBwProbe uint64
	bwProbeUpRounds    uint
	bwProbeUpAcks      congestion.ByteCount
	probeUpCnt         congestion.ByteCount
	bwProbeSamples     bool
	ackPhase           ackPhase

	rs rateSample

	// mpquic: the hooks of a session into the controllers of its paths.

	// AppLimitedHint, if set, reports whether the application currently has
	// no data queued for this connection (C.app_limited; quic-go does not
	// tell its congestion controller). Without it the application is taken
	// to have data all the time.
	AppLimitedHint func() bool
	// ProbeRTTGate, if set, is asked before entering ProbeRTT (enter=true;
	// false defers it to a later ACK) and told when ProbeRTT ends
	// (enter=false), so that only one path of a session probes at a time.
	ProbeRTTGate   func(enter bool) bool
	holdsProbeGate bool
	// RateCap, if set, returns an upper bound for the pacing rate (0:
	// none): the path's share of a configured rate of the line that all
	// paths pass. A known rate is not probed: the bound also holds for
	// the start-up and for ProbeBW_UP.
	RateCap func() Bandwidth
	// LossTolerance, if set and positive, is for a path that is known to
	// lose packets whatever the rate: loss up to this share of what is
	// sent is not taken for congestion, neither when probing (where it
	// replaces BBR.LossThresh if it is more) nor between probes (where the
	// draft answers to any loss). At 1 the controller never answers to
	// loss, as Hysteria's Brutal does not.
	LossTolerance func() float64
}

var _ congestion.CongestionControlEx = &Sender{}

// NewBbrSender returns a BBRv3 controller.
func NewBbrSender(clock Clock, initialMaxDatagramSize congestion.ByteCount) *Sender {
	debug, _ := strconv.ParseBool(os.Getenv(debugEnv))
	b := &Sender{
		clock:   clock,
		sampler: newBandwidthSampler(roundTripCount(extraAckedFilterLen)),
		mds:     initialMaxDatagramSize,
		debug:   debug,
		cwnd:    initialCwndPackets * initialMaxDatagramSize,
	}
	b.pacer = common.NewPacer(b.bandwidthForPacer)
	b.pacer.SetMaxDatagramSize(initialMaxDatagramSize)
	b.onInit()
	return b
}

// ---- the draft: initialization (5.2.1) ----

func (b *Sender) onInit() {
	now := b.clock.Now()
	b.maxBwFilter = [2]Bandwidth{}
	b.maxBw = 0
	b.minRTT = infRTT
	b.minRTTStamp = now
	b.probeRTTMinDelay = infRTT
	b.probeRTTMinStamp = now
	b.probeRTTDoneStamp = 0
	b.probeRTTRoundDone = false
	b.priorCwnd = 0
	b.idleRestart = false
	b.fullBwReached = false
	b.inflightLong = infBytes
	b.resetCongestionSignals()
	b.resetShortTermModel()
	b.nextRoundDlvd, b.roundStart, b.roundCount = 0, false, 0 // BBRInitRoundCounting
	b.resetFullBW()
	b.initPacingRate()
	b.enterStartup()
}

// initPacingRate: the initial window per round trip, at the start-up's
// gain. (The draft takes 1 ms for a round trip that is not known; quic-go
// knows none at this point, and a hundredth of a second is as arbitrary but
// does not send the initial window as one burst.)
func (b *Sender) initPacingRate() {
	srtt := 10 * time.Millisecond
	if b.rttStats != nil && b.rttStats.SmoothedRTT() > 0 {
		srtt = b.rttStats.SmoothedRTT()
	}
	nominal := BandwidthFromDelta(initialCwndPackets*b.mds, srtt)
	b.pacingRate = Bandwidth(startupPacingGain * float64(nominal))
}

// ---- the draft: per-transmit steps (5.2.2, 5.4) ----

// OnPacketSent implements the SendAlgorithm interface.
func (b *Sender) OnPacketSent(sentTime monotime.Time, bytesInFlight congestion.ByteCount, packetNumber congestion.PacketNumber, bytes congestion.ByteCount, isRetransmittable bool) {
	appLimited := b.AppLimitedHint != nil && b.AppLimitedHint()
	// BBRHandleRestartFromIdle
	if bytesInFlight == 0 && appLimited {
		b.idleRestart = true
		if b.isInProbeBW() {
			b.setPacingRateWithGain(1)
		} else if b.st == stProbeRTT {
			b.checkProbeRTTDone(sentTime)
		}
	}
	b.pacer.SentPacket(sentTime, bytes)
	b.inflight = bytesInFlight + bytes
	if appLimited {
		b.sampler.OnAppLimited()
	}
	b.sampler.OnPacketSent(sentTime, packetNumber, bytes, bytesInFlight, isRetransmittable)
}

// CanSend implements the SendAlgorithm interface.
func (b *Sender) CanSend(bytesInFlight congestion.ByteCount) bool {
	if bytesInFlight < b.cwnd {
		return true
	}
	b.cwndLimited = true
	return false
}

// ---- the draft: per-ACK steps (5.2.3) ----

// OnCongestionEventEx implements the CongestionControlEx interface: one
// acknowledgement frame, with what it acknowledged and what was declared
// lost with it.
func (b *Sender) OnCongestionEventEx(priorInFlight congestion.ByteCount, eventTime monotime.Time, ackedPackets []congestion.AckedPacketInfo, lostPackets []congestion.LostPacketInfo) {
	if len(ackedPackets) == 0 && len(lostPackets) == 0 {
		return
	}
	// An application that has nothing queued for this connection limits
	// what the samples of the packets sent from now on can show.
	if b.AppLimitedHint != nil && b.AppLimitedHint() {
		b.sampler.OnAppLimited()
	}

	// GenerateRateSample
	ackedBefore := b.sampler.TotalBytesAcked()
	sample := b.sampler.OnCongestionEvent(eventTime, ackedPackets, lostPackets, b.maxBw, infBandwidth, roundTripCount(b.roundCount))
	rs := rateSample{rtt: sample.sampleRtt}
	for _, p := range ackedPackets {
		rs.newlyAcked += p.BytesAcked
	}
	for _, p := range lostPackets {
		rs.newlyLost += p.BytesLost
	}
	b.inflight = priorInFlight - min(priorInFlight, rs.newlyAcked+rs.newlyLost)
	delivered := b.sampler.TotalBytesAcked() // C.delivered
	if s := sample.lastPacketSendState; s.isValid {
		rs.priorDelivered = s.totalBytesAcked
		rs.txInFlight = s.bytesInFlight
		rs.lost = b.sampler.TotalBytesLost() - s.totalBytesLost
		rs.isAppLimited = s.isAppLimited
	}
	if delivered != ackedBefore {
		// (an ACK of packets that carried data: there are samples)
		rs.valid = true
		rs.deliveryRate = sample.sampleMaxBandwidth
		rs.isAppLimited = sample.sampleIsAppLimited
		rs.delivered = sample.sampleMaxInflight
	}
	b.rs = rs

	if rs.newlyLost > 0 {
		b.lossEvents++
		b.handleLoss(delivered)
	} else if b.inRecovery && rs.priorDelivered >= b.recoveryStart {
		b.inRecovery = false // a packet sent after the loss showed has arrived
	}

	if rs.valid {
		// BBRUpdateModelAndState
		b.updateLatestDeliverySignals(delivered)
		b.updateCongestionSignals(delivered)
		b.updateACKAggregation()
		b.checkFullBWReached()
		b.checkStartupDone()
		b.checkDrainDone()
		b.updateProbeBWCyclePhase(eventTime, delivered)
		b.updateMinRTT(eventTime)
		b.checkProbeRTT(eventTime, delivered)
		b.advanceLatestDeliverySignals()
		b.boundBWForModel()
		// BBRUpdateControlParameters
		b.setPacingRate()
		b.setCwnd(delivered)
	} else {
		// Only losses: the model's bounds may have come down.
		b.boundBWForModel()
		b.boundCwndForModel()
	}
	if b.roundStart {
		b.lossEvents = 0
	}
	b.cwndLimited = false

	// The sampler forgets what is acknowledged or lost. (The least
	// unacknowledged packet is not passed in here; quic-go declares a
	// packet lost at the latest three packets behind an acknowledged one.)
	if len(ackedPackets) != 0 {
		b.sampler.RemoveObsoletePackets(ackedPackets[len(ackedPackets)-1].PacketNumber - 2)
	} else {
		b.sampler.RemoveObsoletePackets(lostPackets[len(lostPackets)-1].PacketNumber + 1)
	}
	if b.debug {
		b.debugPrint("%s bw=%s max=%s short=%s cwnd=%d inflight=%d long=%s short=%s minrtt=%v pacing=%.2f round=%d lost=%d",
			b.st.detail(), formatSpeed(b.bw), formatSpeed(b.maxBw), formatSpeed(b.bwShort), b.cwnd, b.inflight,
			fmtBytes(b.inflightLong), fmtBytes(b.inflightShort), b.minRTT, b.pacingGain, b.roundCount, rs.newlyLost)
	}
}

// ---- the draft: Startup (5.3.1) ----

func (b *Sender) enterStartup() {
	b.st = stStartup
	b.pacingGain = startupPacingGain
	b.cwndGain = defaultCwndGain
}

func (b *Sender) checkStartupDone() {
	b.checkStartupHighLoss()
	if b.st == stStartup && b.fullBwReached {
		b.enterDrain()
	}
}

func (b *Sender) resetFullBW() {
	b.fullBw, b.fullBwCount, b.fullBwNow = 0, 0, false
}

func (b *Sender) checkFullBWReached() {
	if b.fullBwNow || !b.roundStart || b.rs.isAppLimited {
		return // no need to check for a full pipe now
	}
	if float64(b.rs.deliveryRate) >= float64(b.fullBw)*startupGrowth {
		b.resetFullBW() // bw is still growing
		b.fullBw = b.rs.deliveryRate
		return
	}
	b.fullBwCount++ // another round without much growth
	b.fullBwNow = b.fullBwCount >= fullBwRounds
	if b.fullBwNow {
		b.fullBwReached = true
	}
}

// checkStartupHighLoss (5.3.1.3): the start-up is over if the loss rate
// over a full round trip is too high and the losses came in at least six
// separate events (which is what tells overflow from a single burst). The
// safe volume in flight is what the round delivered, or the BDP.
func (b *Sender) checkStartupHighLoss() {
	if b.st != stStartup || !b.roundStart || b.lossEvents < startupFullLoss || !b.isInflightTooHigh() {
		return
	}
	b.fullBwReached = true
	b.inflightLong = max(b.bdpMultiple(1), b.inflightLatest)
}

// ---- the draft: Drain (5.3.2) ----

func (b *Sender) enterDrain() {
	b.st = stDrain
	b.pacingGain = drainPacingGain
	b.cwndGain = defaultCwndGain
	b.drainStart = b.roundCount
}

func (b *Sender) checkDrainDone() {
	if b.st == stDrain && (b.inflight <= b.inflightAt(b.maxBw, 1.0) || b.roundCount > b.drainStart+3) {
		b.enterProbeBW()
	}
}

// ---- the draft: ProbeBW (5.3.3) ----

func (b *Sender) enterProbeBW() {
	b.cwndGain = defaultCwndGain
	b.startProbeBWDown()
}

func (b *Sender) startProbeBWDown() {
	b.resetCongestionSignals()
	b.probeUpCnt = infBytes // not growing inflight_longterm
	b.pickProbeWait()
	b.cycleStamp = b.clock.Now()
	b.ackPhase = acksProbeStopping
	b.startRound()
	b.maxBwAged = false
	b.st = stProbeBWDown
	b.pacingGain, b.cwndGain = probeDownGain, defaultCwndGain
}

func (b *Sender) startProbeBWCruise() {
	b.st = stProbeBWCruise
	b.pacingGain, b.cwndGain = 1, defaultCwndGain
}

func (b *Sender) startProbeBWRefill() {
	// The filter advances once in every cycle (5.5.6). Where the round
	// after ProbeBW_UP could not do it (its sample was application-limited,
	// as after a ProbeRTT that cut the probe short), the start of the next
	// probe does, as in Chromium's BBRv2 (Bbr2ProbeBwMode::ExitProbeDown):
	// otherwise an estimate that the path no longer delivers could stand
	// for good.
	if !b.maxBwAged {
		b.advanceMaxBwFilter()
	}
	b.resetShortTermModel()
	b.bwProbeUpRounds = 0
	b.bwProbeUpAcks = 0
	b.ackPhase = acksRefilling
	b.startRound()
	b.st = stProbeBWRefill
	b.pacingGain, b.cwndGain = 1, defaultCwndGain
}

func (b *Sender) startProbeBWUp() {
	b.ackPhase = acksProbeStarting
	b.startRound()
	b.resetFullBW()
	b.fullBw = b.rs.deliveryRate
	b.st = stProbeBWUp
	b.pacingGain, b.cwndGain = probeUpPacingGain, probeUpCwndGain
	b.raiseInflightLongtermSlope()
}

func (b *Sender) updateProbeBWCyclePhase(now monotime.Time, delivered congestion.ByteCount) {
	if !b.fullBwReached {
		return // only handling steady-state behaviour here
	}
	b.adaptLongTermModel()
	switch b.st {
	case stProbeBWDown:
		if b.isTimeToProbeBW(now) {
			return
		}
		if b.isTimeToCruise() {
			b.startProbeBWCruise()
		}
	case stProbeBWCruise:
		b.isTimeToProbeBW(now)
	case stProbeBWRefill:
		// After one round of REFILL, start UP.
		if b.roundStart {
			b.bwProbeSamples = true
			b.startProbeBWUp()
		}
	case stProbeBWUp:
		if b.isTimeToGoDown() {
			b.startProbeBWDown()
		}
	}
}

func (b *Sender) isInProbeBW() bool {
	return b.st == stProbeBWDown || b.st == stProbeBWCruise || b.st == stProbeBWRefill || b.st == stProbeBWUp
}

func (b *Sender) isTimeToProbeBW(now monotime.Time) bool {
	if now.Sub(b.cycleStamp) > b.bwProbeWait || b.isRenoCoexistenceProbeTime() {
		b.startProbeBWRefill()
		return true
	}
	return false
}

// pickProbeWait: how long to wait until probing for bandwidth again, in
// round trips and in wall clock time, both randomized so that flows do not
// probe in step.
func (b *Sender) pickProbeWait() {
	b.roundsSinceBwProbe = uint64(rand.Intn(2))
	b.bwProbeWait = 2*time.Second + time.Duration(rand.Int63n(int64(time.Second)))
}

func (b *Sender) isRenoCoexistenceProbeTime() bool {
	renoRounds := uint64(b.targetInflight() / b.mds)
	return b.roundsSinceBwProbe >= min(renoRounds, 63)
}

func (b *Sender) targetInflight() congestion.ByteCount {
	return min(b.bdpMultiple(1), b.cwnd)
}

func (b *Sender) isTimeToCruise() bool {
	if b.inflight > b.inflightWithHeadroom() {
		return false // not enough headroom
	}
	if b.inflight > b.inflightAt(b.maxBw, 1.0) {
		return false // more in flight than the estimated BDP
	}
	return true
}

func (b *Sender) isTimeToGoDown() bool {
	if b.cwndLimited && b.cwnd >= b.inflightLong {
		b.resetFullBW() // bw is limited by inflight_longterm
		b.fullBw = b.rs.deliveryRate
	} else if b.fullBwNow {
		return true // we estimate we have fully used the path's bandwidth
	}
	return false
}

func (b *Sender) isProbingBW() bool {
	return b.st == stStartup || b.st == stProbeBWRefill || b.st == stProbeBWUp
}

// inflightWithHeadroom leaves room in the bottleneck's buffer or link for
// other flows, for fairness and for lower delay and loss.
func (b *Sender) inflightWithHeadroom() congestion.ByteCount {
	if b.inflightLong == infBytes {
		return infBytes
	}
	room := max(b.mds, congestion.ByteCount(headroom*float64(b.inflightLong)))
	return max(b.inflightLong-min(room, b.inflightLong), b.minPipeCwnd())
}

func (b *Sender) raiseInflightLongtermSlope() {
	growth := b.mds << b.bwProbeUpRounds
	b.bwProbeUpRounds = min(b.bwProbeUpRounds+1, 30)
	b.probeUpCnt = max(b.cwnd/growth, 1)
}

func (b *Sender) probeInflightLongtermUpward() {
	if !b.cwndLimited || b.cwnd < b.inflightLong {
		return // not fully using inflight_longterm, so don't grow it
	}
	b.bwProbeUpAcks += b.rs.newlyAcked
	if b.bwProbeUpAcks >= b.probeUpCnt {
		delta := b.bwProbeUpAcks / b.probeUpCnt
		b.bwProbeUpAcks -= delta * b.probeUpCnt
		b.inflightLong += delta * b.mds // (the draft counts in packets)
	}
	if b.roundStart {
		b.raiseInflightLongtermSlope()
	}
}

func (b *Sender) adaptLongTermModel() {
	if b.ackPhase == acksProbeStarting && b.roundStart {
		b.ackPhase = acksProbeFeedback // starting to get bw probing samples
	}
	if b.ackPhase == acksProbeStopping && b.roundStart {
		// End of samples from the bw probing phase: once per cycle, one
		// round trip after ProbeBW_UP (5.5.6). The draft's pseudocode
		// does not end the phase here; the Linux implementation does,
		// as below.
		b.bwProbeSamples = false
		b.ackPhase = acksInit
		if b.isInProbeBW() && !b.rs.isAppLimited {
			b.advanceMaxBwFilter()
		}
	}
	if !b.isInflightTooHigh() {
		// The loss rate is safe: adjust the upper bounds upward.
		if b.inflightLong == infBytes {
			return // no upper bounds to raise
		}
		if b.rs.txInFlight > b.inflightLong {
			b.inflightLong = b.rs.txInFlight
		}
		if b.st == stProbeBWUp {
			b.probeInflightLongtermUpward()
		}
	}
}

// ---- the draft: ProbeRTT (5.3.4) ----

func (b *Sender) updateMinRTT(now monotime.Time) {
	b.probeRTTExpired = now.Sub(b.probeRTTMinStamp) > probeRTTInterval
	if b.rs.rtt != infRTT && b.rs.rtt >= 0 && (b.rs.rtt < b.probeRTTMinDelay || b.probeRTTExpired) {
		b.probeRTTMinDelay = b.rs.rtt
		b.probeRTTMinStamp = now
	}
	minRTTExpired := now.Sub(b.minRTTStamp) > minRTTFilterLen
	if b.probeRTTMinDelay < b.minRTT || minRTTExpired {
		b.minRTT = b.probeRTTMinDelay
		b.minRTTStamp = b.probeRTTMinStamp
	}
}

func (b *Sender) checkProbeRTT(now monotime.Time, delivered congestion.ByteCount) {
	if b.st != stProbeRTT && (b.probeRTTExpired || b.probeRTTDeferred) && !b.idleRestart {
		// mpquic: one path of a session at a time. A path that may not
		// enter yet tries again with the next ACK.
		b.probeRTTDeferred = true
		if b.ProbeRTTGate == nil || b.ProbeRTTGate(true) {
			b.probeRTTDeferred = false
			b.holdsProbeGate = b.ProbeRTTGate != nil
			b.enterProbeRTT()
			b.saveCwnd()
			b.probeRTTDoneStamp = 0
			b.ackPhase = acksProbeStopping
			b.startRound()
		}
	}
	if b.st == stProbeRTT {
		b.handleProbeRTT(now)
	}
	if b.rs.delivered > 0 {
		b.idleRestart = false
	}
}

func (b *Sender) enterProbeRTT() {
	b.st = stProbeRTT
	b.pacingGain = 1
	b.cwndGain = probeRTTCwndGain
}

func (b *Sender) handleProbeRTT(now monotime.Time) {
	// Ignore low rate samples during ProbeRTT.
	b.sampler.OnAppLimited()
	if b.probeRTTDoneStamp == 0 && b.inflight <= b.probeRTTCwnd() {
		// Wait for at least ProbeRTTDuration to elapse, and at least one
		// round.
		b.probeRTTDoneStamp = now.Add(probeRTTDuration)
		b.probeRTTRoundDone = false
		b.startRound()
	} else if b.probeRTTDoneStamp != 0 {
		if b.roundStart {
			b.probeRTTRoundDone = true
		}
		if b.probeRTTRoundDone {
			b.checkProbeRTTDone(now)
		}
	}
}

func (b *Sender) checkProbeRTTDone(now monotime.Time) {
	if b.probeRTTDoneStamp != 0 && now > b.probeRTTDoneStamp {
		b.probeRTTMinStamp = now // schedule the next ProbeRTT
		b.restoreCwnd()
		b.exitProbeRTT()
	}
}

func (b *Sender) exitProbeRTT() {
	if b.holdsProbeGate {
		b.holdsProbeGate = false
		b.ProbeRTTGate(false)
	}
	b.resetShortTermModel()
	if b.fullBwReached {
		b.startProbeBWDown()
		b.startProbeBWCruise()
	} else {
		b.enterStartup()
	}
}

// ---- the draft: the network path model (5.5) ----

func (b *Sender) updateRound() {
	if b.rs.priorDelivered >= b.nextRoundDlvd {
		b.startRound()
		b.roundCount++
		b.roundsSinceBwProbe++
		b.roundStart = true
	} else {
		b.roundStart = false
	}
}

func (b *Sender) startRound() {
	b.nextRoundDlvd = b.sampler.TotalBytesAcked()
}

func (b *Sender) updateMaxBw() {
	b.updateRound()
	if r := b.rs.deliveryRate; r > 0 && (r >= b.maxBw || !b.rs.isAppLimited) {
		b.maxBwFilter[0] = max(b.maxBwFilter[0], r)
		b.maxBw = max(b.maxBwFilter[0], b.maxBwFilter[1])
	}
}

// advanceMaxBwFilter: a new ProbeBW cycle. The filter remembers this cycle
// and the one before.
func (b *Sender) advanceMaxBwFilter() {
	if b.maxBwFilter[0] == 0 {
		return // no sample in this cycle: nothing newer to go by
	}
	b.maxBwFilter[1], b.maxBwFilter[0] = b.maxBwFilter[0], 0
	b.maxBw = b.maxBwFilter[1]
	b.maxBwAged = true
}

// updateACKAggregation: BBR.extra_acked is the sampler's figure (it keeps
// the draft's windowed maximum of what was acknowledged beyond the expected
// amount), over one round in the start-up and ten afterwards.
func (b *Sender) updateACKAggregation() {
	n := roundTripCount(1)
	if b.fullBwReached {
		n = extraAckedFilterLen
	}
	b.sampler.SetMaxAckHeightTrackerWindowLength(n)
}

// lossThreshold: BBR.LossThresh, or the path's loss tolerance if that is
// more (mpquic).
func (b *Sender) lossThreshold() float64 {
	return max(lossThresh, b.lossTolerance())
}

func (b *Sender) lossTolerance() float64 {
	if b.LossTolerance == nil {
		return 0
	}
	return b.LossTolerance()
}

// isInflightTooHigh: do loss signals suggest that what was in flight was
// too much? RS.lost is what was lost while the packet just acknowledged was
// under way, RS.tx_in_flight what was in flight when it was sent: the loss
// rate over its round trip.
func (b *Sender) isInflightTooHigh() bool {
	return b.rs.txInFlight > 0 && float64(b.rs.lost) > float64(b.rs.txInFlight)*b.lossThreshold()
}

func (b *Sender) handleInflightTooHigh() {
	b.bwProbeSamples = false // only react once per bw probe
	if !b.rs.isAppLimited {
		b.inflightLong = max(b.rs.txInFlight, congestion.ByteCount(float64(b.targetInflight())*beta))
	}
	if b.st == stProbeBWUp {
		b.startProbeBWDown()
	}
}

// handleLoss is the draft's BBRNoteLoss and BBRHandleLostPacket for the
// packets that this acknowledgement declared lost. The draft goes through
// them one by one, with what was in flight when each was sent, and finds
// the packet at which the loss rate crossed the threshold; quic-go reports
// the losses of an acknowledgement together, so this takes the latest
// packet's figures for all of them (as Chromium's BBRv2 does).
func (b *Sender) handleLoss(delivered congestion.ByteCount) {
	if !b.inRecovery {
		b.inRecovery = true
		b.recoveryStart = delivered
	}
	if !b.lossInRound { // first loss in this round trip
		b.lossRoundDlvd = delivered
	}
	b.lossInRound = true
	if !b.bwProbeSamples {
		return // not a packet sent while probing bandwidth
	}
	if b.isInflightTooHigh() {
		b.handleInflightTooHigh()
	}
}

func (b *Sender) updateLatestDeliverySignals(delivered congestion.ByteCount) {
	b.lossRoundStart = false
	b.bwLatest = max(b.bwLatest, b.rs.deliveryRate)
	b.inflightLatest = max(b.inflightLatest, b.rs.delivered)
	if b.rs.priorDelivered >= b.lossRoundDlvd {
		b.lossRoundDlvd = delivered
		b.lossRoundStart = true
	}
}

func (b *Sender) advanceLatestDeliverySignals() {
	if b.lossRoundStart {
		b.bwLatest = b.rs.deliveryRate
		b.inflightLatest = b.rs.delivered
	}
}

func (b *Sender) resetCongestionSignals() {
	b.lossInRound = false
	b.bwLatest = 0
	b.inflightLatest = 0
}

func (b *Sender) updateCongestionSignals(delivered congestion.ByteCount) {
	b.updateMaxBw()
	if !b.lossRoundStart {
		return // wait until the end of the round trip
	}
	b.adaptLowerBoundsFromCongestion()
	b.lossInRound = false
}

func (b *Sender) adaptLowerBoundsFromCongestion() {
	if b.isProbingBW() {
		return
	}
	// mpquic: on a path with a loss tolerance, the round's loss is taken
	// for congestion only if it is beyond the tolerance.
	if b.lossInRound && (b.lossTolerance() <= 0 || b.isInflightTooHigh()) {
		// BBRInitLowerBounds: the first congestion episode in this cycle
		if b.bwShort == infBandwidth {
			b.bwShort = b.maxBw
		}
		if b.inflightShort == infBytes {
			b.inflightShort = b.cwnd
		}
		// BBRLossLowerBounds
		b.bwShort = max(b.bwLatest, Bandwidth(beta*float64(b.bwShort)))
		b.inflightShort = max(b.inflightLatest, congestion.ByteCount(beta*float64(b.inflightShort)))
	}
}

func (b *Sender) resetShortTermModel() {
	b.bwShort = infBandwidth
	b.inflightShort = infBytes
}

func (b *Sender) boundBWForModel() {
	b.bw = min(b.maxBw, b.bwShort)
}

// ---- the draft: control parameters (5.6) ----

func (b *Sender) setPacingRateWithGain(gain float64) {
	rate := Bandwidth(gain * float64(b.bw) * pacingMargin)
	if b.fullBwReached || rate > b.pacingRate {
		b.pacingRate = rate
	}
}

// bandwidthForPacer is the pacing rate for the pacer, in bytes/s: within
// the session's bound for this path (mpquic), and never zero.

func (b *Sender) setPacingRate() { b.setPacingRateWithGain(b.pacingGain) }

func (b *Sender) bandwidthForPacer() congestion.ByteCount {
	rate := b.pacingRate
	if b.RateCap != nil {
		if c := b.RateCap(); c > 0 {
			rate = min(rate, c)
		}
	}
	return max(congestion.ByteCount(rate/BytesPerSecond), minPacingRate)
}

func (b *Sender) minPipeCwnd() congestion.ByteCount { return minPipeCwndPackets * b.mds }

func (b *Sender) bdpMultiple(gain float64) congestion.ByteCount {
	return b.inflightAt(b.bw, gain)
}

// inflightAt is BBRInflight for a bandwidth: gain times its BDP, and at
// least the minimum for pipelining. (No offload budget: quic-go sends
// packet by packet.)
func (b *Sender) inflightAt(bw Bandwidth, gain float64) congestion.ByteCount {
	if b.minRTT == infRTT {
		return initialCwndPackets * b.mds // no valid RTT samples yet
	}
	bdp := float64(bw/BytesPerSecond) * b.minRTT.Seconds()
	v := max(congestion.ByteCount(gain*bdp), b.minPipeCwnd())
	if b.st == stProbeBWUp {
		v += 2 * b.mds
	}
	return v
}

func (b *Sender) updateMaxInflight() {
	if b.minRTT == infRTT {
		b.maxInflight = initialCwndPackets * b.mds
		return
	}
	bdp := float64(b.bw/BytesPerSecond) * b.minRTT.Seconds()
	v := congestion.ByteCount(b.cwndGain*bdp) + b.sampler.MaxAckHeight()
	v = max(v, b.minPipeCwnd())
	if b.st == stProbeBWUp {
		v += 2 * b.mds
	}
	b.maxInflight = v
}

func (b *Sender) saveCwnd() {
	if !b.inRecovery && b.st != stProbeRTT {
		b.priorCwnd = b.cwnd
	} else {
		b.priorCwnd = max(b.priorCwnd, b.cwnd)
	}
}

func (b *Sender) restoreCwnd() { b.cwnd = max(b.cwnd, b.priorCwnd) }

func (b *Sender) probeRTTCwnd() congestion.ByteCount {
	return max(b.bdpMultiple(probeRTTCwndGain), b.minPipeCwnd())
}

func (b *Sender) setCwnd(delivered congestion.ByteCount) {
	b.updateMaxInflight()
	if b.fullBwReached {
		b.cwnd = min(b.cwnd+b.rs.newlyAcked, b.maxInflight)
	} else if b.cwnd < b.maxInflight || delivered < initialCwndPackets*b.mds {
		b.cwnd += b.rs.newlyAcked
	}
	b.cwnd = max(b.cwnd, b.minPipeCwnd())
	if b.st == stProbeRTT { // BBRBoundCwndForProbeRTT
		b.cwnd = min(b.cwnd, b.probeRTTCwnd())
	}
	b.boundCwndForModel()
}

func (b *Sender) boundCwndForModel() {
	limit := infBytes
	if b.isInProbeBW() && b.st != stProbeBWCruise {
		limit = b.inflightLong
	} else if b.st == stProbeRTT || b.st == stProbeBWCruise {
		limit = b.inflightWithHeadroom()
	}
	limit = min(limit, b.inflightShort)
	limit = max(limit, b.minPipeCwnd())
	b.cwnd = min(b.cwnd, limit)
}

// ---- the rest of quic-go's interface ----

func (b *Sender) SetRTTStatsProvider(provider congestion.RTTStatsProvider) {
	b.rttStats = provider
	if b.maxBw == 0 {
		b.initPacingRate()
	}
}

func (b *Sender) TimeUntilSend(congestion.ByteCount) monotime.Time { return b.pacer.TimeUntilSend() }

func (b *Sender) HasPacingBudget(now monotime.Time) bool {
	return b.pacer.Budget(now) >= b.mds
}

func (b *Sender) MaybeExitSlowStart() {}

func (b *Sender) OnPacketAcked(congestion.PacketNumber, congestion.ByteCount, congestion.ByteCount, monotime.Time) {
}

func (b *Sender) OnCongestionEvent(congestion.PacketNumber, congestion.ByteCount, congestion.ByteCount) {
}

// OnRetransmissionTimeout: quic-go's probe timeout fired. (The draft's
// BBROnEnterRTO cuts the window to what is in flight; quic-go sends its
// probe packets regardless of the window, and what they find comes in
// through the next acknowledgement.)
func (b *Sender) OnRetransmissionTimeout(bool) {}

func (b *Sender) SetMaxDatagramSize(s congestion.ByteCount) {
	if s < b.mds {
		panic(fmt.Sprintf("congestion BUG: decreased max datagram size from %d to %d", b.mds, s))
	}
	if b.cwnd == initialCwndPackets*b.mds {
		b.cwnd = initialCwndPackets * s
	}
	b.mds = s
	b.pacer.SetMaxDatagramSize(s)
}

func (b *Sender) InSlowStart() bool                         { return b.st == stStartup }
func (b *Sender) InRecovery() bool                          { return b.inRecovery }
func (b *Sender) GetCongestionWindow() congestion.ByteCount { return b.cwnd }

// ---- what the session reads (mpquic) ----

// ModeName is the state for the scheduler and the logs: startup, drain,
// probe_bw or probe_rtt.
func (b *Sender) ModeName() string { return b.st.String() }

// BandwidthEstimate is BBR.bw in bytes/s.
func (b *Sender) BandwidthEstimate() uint64 { return uint64(b.bw / BytesPerSecond) }

// PacingRateBytes is the pacing rate in bytes/s.
func (b *Sender) PacingRateBytes() uint64 { return uint64(b.bandwidthForPacer()) }

// MinRTT is BBR.min_rtt (0: not known).
func (b *Sender) MinRTT() time.Duration {
	if b.minRTT == infRTT {
		return 0
	}
	return b.minRTT
}

func (b *Sender) debugPrint(format string, a ...any) {
	fmt.Printf("[BBR] [%s] %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
}

func fmtBytes(v congestion.ByteCount) string {
	if v == infBytes {
		return "inf"
	}
	return strconv.FormatInt(int64(v), 10)
}

func formatSpeed(bw Bandwidth) string {
	if bw == infBandwidth {
		return "inf"
	}
	bps := float64(bw)
	switch {
	case bps >= 1e6:
		return fmt.Sprintf("%.2fMbps", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.2fKbps", bps/1e3)
	}
	return fmt.Sprintf("%.0fbps", bps)
}
