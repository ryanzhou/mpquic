package mp

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestSendQueueCarving(t *testing.T) {
	for seed := int64(0); seed < 50; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var q sendQueue
		var want []byte
		got := map[uint64][]byte{}
		finSeen := false
		total := 200000 + rng.Intn(100000)
		for len(want) < total || q.avail() > 0 || q.finPending {
			if len(want) < total && rng.Intn(3) == 0 {
				b := make([]byte, 1+rng.Intn(5000))
				rng.Read(b)
				q.push(b, 16384)
				want = append(want, b...)
				if len(want) >= total {
					q.finPending = true
				}
				continue
			}
			if q.avail() == 0 && !q.finPending {
				continue
			}
			n := 1 + rng.Intn(3000)
			front := q.frontOff()
			if rng.Intn(3) == 0 && q.avail() > int64(3*n) {
				if off, ok := q.aheadSlot(front+uint64(rng.Intn(50000)), n, q.end); ok {
					got[off] = q.carveAt(off, n)
					continue
				}
			}
			off, data, fin := q.carveFront(n)
			if len(data) > 0 {
				got[off] = data
			}
			if fin {
				if off+uint64(len(data)) != uint64(len(want)) {
					t.Fatalf("seed %d: fin at %d, stream length %d", seed, off+uint64(len(data)), len(want))
				}
				finSeen = true
			}
		}
		if !finSeen {
			t.Fatalf("seed %d: no fin", seed)
		}
		// reassemble: every byte exactly once
		var out []byte
		for len(out) < len(want) {
			d, ok := got[uint64(len(out))]
			if !ok {
				t.Fatalf("seed %d: gap at %d", seed, len(out))
			}
			out = append(out, d...)
		}
		if !bytes.Equal(out, want) {
			t.Fatalf("seed %d: data mismatch", seed)
		}
	}
}

func TestAheadSlotFindsGaps(t *testing.T) {
	var q sendQueue
	q.push(make([]byte, 1<<20), 16384)
	// fill many ahead regions with small gaps between them
	for off := uint64(100000); off < 900000; off += 20000 {
		q.carveAt(off, 16000)
	}
	for _, target := range []uint64{50000, 100000, 101000, 500000, 899000, 2 << 20} {
		off, ok := q.aheadSlot(target, 4000, q.end)
		if !ok {
			t.Fatalf("no slot near %d", target)
		}
		for _, r := range q.ahead {
			if off < r.end && off+4000 > r.start {
				t.Fatalf("slot %d overlaps %v", off, r)
			}
		}
		if off < q.front+4000 || off+4000 > q.end {
			t.Fatalf("slot %d out of bounds", off)
		}
	}
}
