package mp

import "math"

// A configured rate
//
// Every path has its own congestion controller (BBR, see internal/cc/bbr),
// which finds what that path carries; the session adds nothing to that. The
// exception is a rate that is known: Options.MaxSendRate (this endpoint's
// uplink) or what the peer announces as its MaxRecvRate (its downlink).
// The paths together then send no more than that, so that the line is
// never filled to find out what it carries.
//
// The limit is shared out max-min fairly, as bounds on the paths' sending
// rates (bbr.Sender.RateCap). What a path can use of it is known: outside
// its start-up, BBR never sends faster than 1.25 times its estimate (its
// probing gain). A path that can use less than an equal share is bound to
// that, and the others share the rest equally; as its estimate grows, so
// does its bound, up to an equal share.

// limitDemandGain: BBR's highest pacing gain after the start-up.
const limitDemandGain = 1.25

// limitMinShare: the least a path is bound to (bytes/s), so that a path
// without an estimate yet can get started. (0 means no bound.)
const limitMinShare = 16 << 10

// rateLimitLocked is the configured ceiling of the session's send rate
// (bytes/s, 0 = none): our own limit and what the peer wants to receive.
func (s *Session) rateLimitLocked() uint64 {
	l := s.opt.MaxSendRate
	if p := s.peerLimit; p > 0 && (l == 0 || p < l) {
		l = p
	}
	return l
}

// shareLimitLocked sets every path's share of the configured limit.
func (s *Session) shareLimitLocked() {
	limit := float64(s.rateLimitLocked())
	open := s.tmpPaths[:0]
	for _, p := range s.paths {
		switch {
		case p.meter == nil || p.state == stateDead:
		case limit == 0 || p.state != stateActive:
			// (a path that carries no data: what it sends to find out
			// whether it is alive is not worth a share)
			p.setRateCapLocked(0)
		default:
			open = append(open, p)
		}
	}
	left := limit
	for changed := true; changed && len(open) > 0; {
		changed = false
		fair := left / float64(len(open))
		k := 0
		for _, p := range open {
			if use := limitDemandGain * float64(p.bw); p.ccMode != "startup" && use <= fair {
				p.setRateCapLocked(use)
				left -= use
				changed = true
			} else {
				open[k], k = p, k+1
			}
		}
		open = open[:k]
	}
	for _, p := range open {
		p.setRateCapLocked(left / float64(len(open)))
	}
	clear(s.tmpPaths[:cap(s.tmpPaths)])
	s.tmpPaths = open[:0]
}

func (p *Path) setRateCapLocked(c float64) {
	v := uint64(math.Ceil(c))
	if c > 0 {
		v = max(v, limitMinShare)
	}
	if p.rateCap != v {
		p.rateCap = v
		p.meter.SetRateCap(v)
	}
}
