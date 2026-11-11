package cc

import (
	"testing"
	"time"

	"github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"
)

type fixedRTT time.Duration

func (r fixedRTT) MinRTT() time.Duration        { return time.Duration(r) }
func (r fixedRTT) LatestRTT() time.Duration     { return time.Duration(r) }
func (r fixedRTT) SmoothedRTT() time.Duration   { return time.Duration(r) }
func (r fixedRTT) MeanDeviation() time.Duration { return 0 }
func (r fixedRTT) MaxAckDelay() time.Duration   { return 0 }
func (r fixedRTT) PTO(bool) time.Duration       { return 2 * time.Duration(r) }
func (r fixedRTT) UpdateRTT(_, _ time.Duration) {}
func (r fixedRTT) SetMaxAckDelay(time.Duration) {}
func (r fixedRTT) SetInitialRTT(time.Duration)  {}

// A follower has what its rate sends in two round trips in flight, or in
// one round trip and 50 ms if that is more; and it says when that held it
// back.
func TestFollowerWindow(t *testing.T) {
	m := NewFollower()
	m.SetRTTStatsProvider(fixedRTT(100 * time.Millisecond))
	m.SetRate(1 << 20)
	if w := m.GetCongestionWindow(); w != (1<<20)/5 {
		t.Errorf("window %d, want %d (two round trips)", w, (1<<20)/5)
	}
	if !m.CanSend(100<<10) || m.TakeBlocked() {
		t.Error("blocked below the window")
	}
	if m.CanSend(300<<10) || !m.TakeBlocked() || m.TakeBlocked() {
		t.Error("not blocked (once) at the window")
	}
	// a short path: one round trip and 50 ms
	m.SetRTTStatsProvider(fixedRTT(10 * time.Millisecond))
	if w, want := m.GetCongestionWindow(), congestion.ByteCount((1<<20)*60/1000); w != want {
		t.Errorf("window %d, want %d (one round trip and 50 ms)", w, want)
	}
	// the session's base RTT counts, not the connection's lifetime minimum
	m.SetBaseRTT(200 * time.Millisecond)
	if w, want := m.GetCongestionWindow(), congestion.ByteCount((1<<20)*2/5); w != want {
		t.Errorf("window %d, want %d (the session's base RTT)", w, want)
	}
	// never less than a few packets
	m.SetRate(1)
	if w := m.GetCongestionWindow(); w < 16*1200 {
		t.Errorf("window %d at the lowest rate", w)
	}
	if s := m.Snapshot(); s.Mode != "follow" || s.Bandwidth != 1 {
		t.Errorf("snapshot %+v", s)
	}
}

// Every packet's fate is counted for the round it was sent in, however much
// later it is known.
func TestMeterRounds(t *testing.T) {
	m := NewFollower()
	at := func(ms int) monotime.Time { return monotime.Time(int64(time.Hour) + int64(ms)*int64(time.Millisecond)) }
	pn := congestion.PacketNumber(0)
	send := func(ms int, n int) (first congestion.PacketNumber) {
		first = pn
		for i := 0; i < n; i++ {
			m.OnPacketSent(at(ms), 0, pn, 1000, true)
			pn++
		}
		return first
	}
	ack := func(ms int, from congestion.PacketNumber, n int, lost bool) {
		var a []congestion.AckedPacketInfo
		var l []congestion.LostPacketInfo
		for i := 0; i < n; i++ {
			if lost {
				l = append(l, congestion.LostPacketInfo{PacketNumber: from + congestion.PacketNumber(i), BytesLost: 1000})
			} else {
				a = append(a, congestion.AckedPacketInfo{PacketNumber: from + congestion.PacketNumber(i), BytesAcked: 1000})
			}
		}
		m.OnCongestionEventEx(0, at(ms), a, l)
	}
	m.StartRound(0)
	r0 := send(0, 10)
	m.OnPacketSent(at(1), 0, pn, 50, false) // an ACK-only packet does not count
	pn++
	m.StartRound(1)
	r1 := send(100, 20)
	m.StartRound(2) // nothing sent in round 2
	m.StartRound(3)
	r3 := send(300, 5)

	ack(110, r0, 8, false)
	ack(120, r0+8, 2, true)
	ack(210, r1, 10, false)
	ack(320, r1+10, 10, false) // late: still round 1's
	ack(400, r3, 5, false)

	for _, c := range []struct {
		round             int64
		sent, acked, lost uint64
	}{{0, 10000, 8000, 2000}, {1, 20000, 20000, 0}, {2, 0, 0, 0}, {3, 5000, 5000, 0}, {4, 0, 0, 0}, {-1, 0, 0, 0}} {
		if s := m.RoundStats(c.round); s.Sent != c.sent || s.Acked != c.acked || s.Lost != c.lost {
			t.Errorf("round %d: %+v, want sent %d acked %d lost %d", c.round, s, c.sent, c.acked, c.lost)
		}
	}
	if a := m.Acked(); a != 33000 {
		t.Errorf("acked %d", a)
	}
	// Round 1 was sent in 100 ms, and its acknowledgements ended 210 ms
	// after round 0's: it queued, and was delivered at 20000 B / 210 ms.
	s0, s1 := m.RoundStats(0), m.RoundStats(1)
	if r, want := s1.Rate(s0, 100*time.Millisecond), 20000/0.21; r < want-1 || r > want+1 {
		t.Errorf("rate %.0f, want %.0f", r, want)
	}
	// Without a round before it (or acknowledgements that came faster than
	// the round was sent), a round's own length counts.
	if r := s0.Rate(RoundStat{}, 100*time.Millisecond); r != 80000 {
		t.Errorf("rate %.0f, want 80000", r)
	}
	// Old rounds are forgotten, and their slots reused.
	for i := int64(4); i < 40; i++ {
		m.StartRound(i)
		send(int(i)*100, 1)
	}
	if s := m.RoundStats(3); s.Sent != 0 {
		t.Errorf("round 3 after 36 more: %+v", s)
	}
	if s := m.RoundStats(39); s.Sent != 1000 {
		t.Errorf("round 39: %+v", s)
	}
}
