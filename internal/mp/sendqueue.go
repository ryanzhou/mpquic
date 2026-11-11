package mp

import "sort"

// sendQueue holds a stream's not-yet-carved bytes. Chunks are normally carved
// from the front, but a slow path may carve a region further ahead (see
// lookaheadLocked); front carving then skips such regions.
type sendQueue struct {
	bufs   [][]byte // buffers holding bytes [bufOff[0], end)
	bufOff []uint64 // stream offset of bufs[i][0]
	end    uint64   // offset after the last queued byte (= writeOff)
	front  uint64   // first byte not carved at the front

	ahead      []ackRange // carved-ahead regions beyond front, sorted, disjoint
	aheadBytes int64

	finPending bool
}

// avail is the number of queued bytes not carved yet.
func (q *sendQueue) avail() int64 { return int64(q.end-q.front) - q.aheadBytes }

// push appends p (copied), filling buffers of up to chunk bytes.
func (q *sendQueue) push(p []byte, chunk int) {
	for len(p) > 0 {
		if l := len(q.bufs); l > 0 {
			last := q.bufs[l-1]
			if room := cap(last) - len(last); room > 0 {
				k := min(room, len(p))
				q.bufs[l-1] = append(last, p[:k]...)
				p = p[k:]
				q.end += uint64(k)
				continue
			}
		}
		// A full-size buffer for every small write (a keystroke, a
		// datagram in reliable UDP mode) was 16 KB allocated per 100 bytes
		// sent. Room for a few more writes of the size is enough to
		// coalesce them.
		b := make([]byte, 0, min(chunk, max(512, 4*len(p))))
		q.bufs = append(q.bufs, b)
		q.bufOff = append(q.bufOff, q.end)
	}
}

// bytesAt returns n bytes starting at off (zero-copy if within one buffer).
func (q *sendQueue) bytesAt(off uint64, n int) []byte {
	if n == 0 {
		return nil
	}
	i := sort.Search(len(q.bufOff), func(i int) bool { return q.bufOff[i] > off }) - 1
	start := int(off - q.bufOff[i])
	if start+n <= len(q.bufs[i]) {
		return q.bufs[i][start : start+n : start+n]
	}
	out := make([]byte, 0, n)
	for len(out) < n {
		k := min(n-len(out), len(q.bufs[i])-start)
		out = append(out, q.bufs[i][start:start+k]...)
		i, start = i+1, 0
	}
	return out
}

// release drops buffers entirely below the front.
func (q *sendQueue) release() {
	k := 0
	for k < len(q.bufs) && q.bufOff[k]+uint64(len(q.bufs[k])) <= q.front && (k < len(q.bufs)-1 || q.front == q.end) {
		q.bufs[k] = nil
		k++
	}
	if k > 0 {
		q.bufs, q.bufOff = q.bufs[k:], q.bufOff[k:]
	}
}

// skipAhead advances the front past carved-ahead regions that start at it.
func (q *sendQueue) skipAhead() {
	for len(q.ahead) > 0 && q.ahead[0].start <= q.front {
		q.aheadBytes -= int64(q.ahead[0].end - q.ahead[0].start)
		q.front = max(q.front, q.ahead[0].end)
		q.ahead = q.ahead[1:]
	}
	q.release()
}

// frontOff is the offset the next front chunk would start at.
func (q *sendQueue) frontOff() uint64 {
	q.skipAhead()
	return q.front
}

// carveFront takes up to n bytes from the front (not crossing a carved-ahead
// region). fin is set if the chunk ends the stream.
func (q *sendQueue) carveFront(n int) (off uint64, data []byte, fin bool) {
	q.skipAhead()
	limit := q.end - q.front
	if len(q.ahead) > 0 {
		limit = q.ahead[0].start - q.front
	}
	n = int(min(uint64(n), limit))
	off = q.front
	data = q.bytesAt(off, n)
	q.front += uint64(n)
	if q.finPending && off+uint64(n) == q.end {
		fin = true
		q.finPending = false
	}
	q.skipAhead()
	return off, data, fin
}

// aheadSlot finds where to carve n bytes ahead: the first free gap at or
// after target, or else the latest free gap before it. The region must lie in
// the uncarved bytes below limit and not touch the front itself.
func (q *sendQueue) aheadSlot(target uint64, n int, limit uint64) (uint64, bool) {
	limit = min(limit, q.end)
	N := uint64(n)
	lo := q.front + N // never directly at the front
	if limit < lo+N {
		return 0, false
	}
	target = min(max(target, lo), limit-N)
	// forward: gaps starting at target or right after a region
	i := sort.Search(len(q.ahead), func(i int) bool { return q.ahead[i].end > target })
	start := target
	for {
		next := limit
		if i < len(q.ahead) {
			next = min(next, q.ahead[i].start)
		}
		if start+N <= next {
			return start, true
		}
		if i >= len(q.ahead) {
			break
		}
		start = max(start, q.ahead[i].end)
		i++
		if start+N > limit {
			break
		}
	}
	// backward: latest gap [prevEnd, regionStart) below target
	j := sort.Search(len(q.ahead), func(i int) bool { return q.ahead[i].start >= target })
	for k := j; k >= 0; k-- {
		gapEnd := target + N
		if k < len(q.ahead) {
			gapEnd = min(gapEnd, q.ahead[k].start)
		}
		gapStart := lo
		if k > 0 {
			gapStart = max(lo, q.ahead[k-1].end)
		}
		if gapEnd >= gapStart+N {
			return gapEnd - N, true
		}
	}
	return 0, false
}

// carveAt marks [off, off+n) as carved and returns its bytes.
func (q *sendQueue) carveAt(off uint64, n int) []byte {
	data := q.bytesAt(off, n)
	r := ackRange{off, off + uint64(n)}
	i := sort.Search(len(q.ahead), func(i int) bool { return q.ahead[i].start > off })
	q.ahead = append(q.ahead, ackRange{})
	copy(q.ahead[i+1:], q.ahead[i:])
	q.ahead[i] = r
	q.aheadBytes += int64(n)
	return data
}
