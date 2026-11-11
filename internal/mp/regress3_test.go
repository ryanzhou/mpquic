package mp

import (
	"bufio"
	"bytes"
	"io"
	"testing"
	"time"
)

// The remote aborts a stream (e.g. dial failure) while the only path is down.
func TestRegressResetLostWithoutPath(t *testing.T) {
	goAhead := make(chan struct{})
	handler := func(st *Stream) {
		b := make([]byte, 5)
		io.ReadFull(st, b)
		<-goAhead
		st.Reset(ResetDialFailed)
		st.Close()
	}
	tb := newTestbed(t, []string{"delay=2ms"}, handler, func(o *Options) {
		o.SuspectTimeout = 300 * time.Millisecond
		o.PathDeadTimeout = time.Second
	})
	st, _ := tb.cl.OpenStream()
	st.Write([]byte("hello"))
	time.Sleep(200 * time.Millisecond)
	tb.relays[0].SetDown(true)
	srv := tb.srv.Session()
	for i := 0; ; i++ {
		srv.mu.Lock()
		n := len(srv.paths)
		srv.mu.Unlock()
		if n == 0 {
			break
		}
		if i > 500 {
			t.Fatal("server path did not die")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(goAhead) // Reset with no path
	time.Sleep(100 * time.Millisecond)
	tb.relays[0].SetDown(false)
	tb.waitPaths(t, 1)
	t.Logf("path is back")
	res := make(chan error, 1)
	start := time.Now()
	go func() { _, err := io.ReadAll(st); res <- err }()
	select {
	case err := <-res:
		t.Logf("client stream ended after %v: %v", time.Since(start).Round(time.Millisecond), err)
	case <-time.After(15 * time.Second):
		srv.mu.Lock()
		ns := len(srv.streams)
		srv.mu.Unlock()
		t.Errorf("STALLED: 15 s after the path returned the client stream still waits (server has %d streams; its RESET was never sent)", ns)
	}
}

// An ACK whose SACK ranges were cut to the size of a datagram must not look
// complete to the sender: it would resend what the receiver already has.
func TestRegressSackTruncation(t *testing.T) {
	for _, n := range []int{10, 47, 48, 100, 112, 150, 200} {
		e := ackEntry{id: 5, cum: 1 << 20, window: 64 << 20}
		off := e.cum + 16384
		for i := 0; i < n; i++ {
			e.ranges = append(e.ranges, ackRange{off, off + 16384})
			off += 2 * 16384
		}
		dgs := encodeAckDatagrams([]ackEntry{e})
		if len(dgs) != 1 {
			t.Fatalf("datagrams: %d", len(dgs))
		}
		typ, body, err := readFrame(bufio.NewReader(bytes.NewReader(dgs[0][1:])))
		if err != nil || typ != frAck {
			t.Fatal(err)
		}
		got, err := decodeAck(body)
		if err != nil {
			t.Fatal(err)
		}
		s := newSession((&Options{}).withDefaults(), [16]byte{1}, true, nil)
		st, _ := s.OpenStream()
		s.mu.Lock()
		st.onAckLocked(&got[0])
		complete := st.sackLimit == ^uint64(0)
		s.mu.Unlock()
		s.Close()
		if cut := len(got[0].ranges) < n; cut && complete {
			t.Errorf("%d ranges were cut to %d, and the sender takes the list for complete", n, len(got[0].ranges))
		}
		if n >= 100 && len(got[0].ranges) < 90 {
			t.Errorf("only %d of %d ranges fit: the datagram has room for more", len(got[0].ranges), n)
		}
	}
}
