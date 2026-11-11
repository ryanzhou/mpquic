package mp

// Regression tests for bugs found in review: each failed before its fix.

import (
	"io"
	"testing"
	"time"
)

// A paused consumer, then a short blackout while it drains its buffer: the
// window updates are lost; does the stream ever resume?
func TestRegressWindowUpdateLost(t *testing.T) {
	handler := func(st *Stream) {
		buf := make([]byte, 32<<10)
		for {
			if _, err := st.Write(buf); err != nil {
				return
			}
		}
	}
	tb := newTestbed(t, []string{"delay=5ms,rate=80mbit"}, handler, nil)
	st, _ := tb.cl.OpenStream()
	st.Write([]byte{1})
	s := tb.cl.Session()
	// the consumer is paused: wait until the sender has filled the window
	deadline := time.Now().Add(15 * time.Second)
	for {
		s.mu.Lock()
		rb, rw := st.readyBytes, st.recvWindow
		s.mu.Unlock()
		if uint64(rb) >= rw {
			t.Logf("window full: %d bytes buffered, recvWindow %d", rb, rw)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("window did not fill: %d of %d", rb, rw)
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(800 * time.Millisecond) // sender is blocked; no data for > 500 ms
	tb.relays[0].SetDown(true)
	buf := make([]byte, 8<<20)
	n, err := io.ReadFull(st, buf) // the consumer resumes and drains its buffer
	t.Logf("consumer read %d bytes during the blackout (err=%v)", n, err)
	time.Sleep(250 * time.Millisecond)
	tb.relays[0].SetDown(false)
	got := make(chan int, 1)
	start := time.Now()
	go func() { n, _ := st.Read(buf); got <- n }()
	select {
	case n := <-got:
		t.Logf("stream resumed after %v (%d bytes)", time.Since(start).Round(time.Millisecond), n)
	case <-time.After(20 * time.Second):
		t.Errorf("the stream did not resume after the window update was lost")
		s.mu.Lock()
		srv := tb.srv.Session()
		t.Logf("STALLED: no data 20 s after a 250 ms blackout; client: consumed=%d recvOff=%d advertised window=%d, path states=%v",
			st.consumed, st.recvOff, st.lastAdvWindow, s.statsStringLocked())
		s.mu.Unlock()
		srv.mu.Lock()
		for _, x := range srv.streams {
			t.Logf("server stream %d: peerWindow=%d front=%d unsent=%d unacked chunks=%d", x.id, x.peerWindow, x.sq.frontOff(), x.unsent, len(x.sendBuf))
		}
		srv.mu.Unlock()
		t.Fail()
	}
}

// A paused consumer on a session with reordering: how large does the
// receive window (= what the receiver buffers) get?
func TestRegressWindowGrowsForSlowConsumer(t *testing.T) {
	s := newSession((&Options{}).withDefaults(), [16]byte{2}, false, func(*Stream) {})
	defer s.Close()
	p := &Path{}
	chunk := make([]byte, 16<<10)
	off := uint64(0)
	var st *Stream
	for i := 0; ; i++ {
		s.mu.Lock()
		if st == nil {
			st = s.streams[7]
		}
		var win uint64
		var ready, ooo int64
		if st != nil {
			win, ready, ooo = st.consumed+st.recvWindow, st.readyBytes, st.oooBytes
		}
		s.mu.Unlock()
		if st != nil && off+3*uint64(len(chunk)) > win {
			t.Logf("sender stopped by the window at %d MB: recvWindow=%d MB, buffered ready=%d MB, out of order=%d KB, consumed=0",
				off>>20, st.recvWindow>>20, ready>>20, ooo>>10)
			if st.recvWindow > 8<<20 {
				t.Errorf("the window of a consumer that never reads grew to %d MB", st.recvWindow>>20)
			}
			break
		}
		// mild reordering: every chunk pair arrives swapped; the app never reads
		s.onChunk(p, 7, off+uint64(len(chunk)), chunk, false)
		s.onChunk(p, 7, off, chunk, false)
		off += 2 * uint64(len(chunk))
	}
}

// An application-limited upload (2 Mbit/s) over two 5 Mbit/s paths: how much
// of it is sent twice?
func TestRegressEagerDupOfBulk(t *testing.T) {
	sink := func(st *Stream) { io.Copy(io.Discard, st); st.Close() }
	tb := newTestbed(t, []string{"delay=5ms,rate=5mbit", "delay=8ms,rate=5mbit"}, sink, nil)
	st, _ := tb.cl.OpenStream()
	buf := make([]byte, 25<<10)
	for i := 0; i < 60; i++ {
		st.Write(buf)
		time.Sleep(100 * time.Millisecond)
	}
	var sent, dup, reinj uint64
	for _, ps := range tb.cl.Session().Stats() {
		sent += ps.SentBytes
		dup += ps.DupBytes
		reinj += ps.ReinjectBytes
		t.Logf("path %s: sent=%dKB dup=%dKB reinject=%dKB loss=%.3f bw=%.1fMbit", ps.Name, ps.SentBytes>>10, ps.DupBytes>>10, ps.ReinjectBytes>>10, ps.Loss, float64(ps.Bandwidth)*8/1e6)
	}
	t.Logf("application wrote %d KB; paths sent %d KB; duplicates %d KB (%.0f%%), reinjections %d KB", 60*25, sent>>10, dup>>10, 100*float64(dup)/float64(60*25<<10), reinj>>10)
	if float64(dup) > 0.15*float64(60*25<<10) {
		t.Errorf("%d KB of a 1500 KB bulk upload were sent twice: bulk data is duplicated like small messages", dup>>10)
	}
}
