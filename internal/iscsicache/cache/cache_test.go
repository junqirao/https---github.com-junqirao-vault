package cache

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeSource serves a synthetic pattern in which every byte equals the index of
// the sector it belongs to, so a reader can verify it got the right offsets.
type fakeSource struct {
	sectorSize int
	size       int64

	mu    sync.Mutex
	reads int
	bytes int64
}

func newFakeSource(sectorSize int, size int64) *fakeSource {
	return &fakeSource{sectorSize: sectorSize, size: size}
}

func (f *fakeSource) ReadAt(_ context.Context, off int64, p []byte) error {
	f.mu.Lock()
	f.reads++
	f.bytes += int64(len(p))
	f.mu.Unlock()
	if off < 0 || off+int64(len(p)) > f.size {
		return io.ErrUnexpectedEOF
	}
	for i := range p {
		p[i] = byte((off + int64(i)) / int64(f.sectorSize))
	}
	return nil
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func expectPattern(t *testing.T, sectorSize int, off int64, got []byte) {
	t.Helper()
	want := make([]byte, len(got))
	for i := range want {
		want[i] = byte((off + int64(i)) / int64(sectorSize))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("data at offset %d does not match the source pattern", off)
	}
}

type testOpts struct {
	blockSize  int
	sectorSize int
	l1Bytes    int64
	shards     int
	scan       bool
	threshold  int
	l2         L2
}

func newTestCache(t *testing.T, o testOpts) *Cache {
	t.Helper()
	if o.blockSize == 0 {
		o.blockSize = 16 << 10
	}
	if o.sectorSize == 0 {
		o.sectorSize = 4 << 10
	}
	if o.shards == 0 {
		o.shards = 4
	}
	if o.l1Bytes == 0 {
		o.l1Bytes = 1 << 20
	}
	c, err := New(Config{
		BlockSize:     o.blockSize,
		SectorSize:    o.sectorSize,
		L1Bytes:       o.l1Bytes,
		Shards:        o.shards,
		ScanEnabled:   o.scan,
		ScanThreshold: o.threshold,
		L2:            o.l2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestReadFillsL1AndHits(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 4<<20)
	c := newTestCache(t, testOpts{})
	ctx := context.Background()

	buf := make([]byte, ss)
	if err := c.Read(ctx, 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	expectPattern(t, ss, 0, buf)
	if src.count() != 1 {
		t.Fatalf("backend reads after a miss = %d, want 1", src.count())
	}
	// The fill is asynchronous: wait for it before expecting a hit.
	c.Wait()

	// Only the requested sector is filled: the cache does not prefetch the rest
	// of the block.
	buf2 := make([]byte, ss)
	if err := c.Read(ctx, 0, buf2, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	expectPattern(t, ss, 0, buf2)
	if src.count() != 1 {
		t.Fatalf("backend reads after an L1 hit = %d, want 1", src.count())
	}
	if st := c.Stats(); st.L1Hits != 1 || st.Fills != 1 {
		t.Fatalf("stats = %+v", st)
	}

	// A neighbouring sector of the same block is a partial hit and must be
	// fetched.
	if err := c.Read(ctx, 2*ss, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	expectPattern(t, ss, 2*ss, buf)
	if src.count() != 2 {
		t.Fatalf("backend reads after a partial hit = %d, want 2", src.count())
	}
	c.Wait()
	if st := c.Stats(); st.PartialHits != 1 || st.Fills != 2 {
		t.Fatalf("stats = %+v", st)
	}

	// Both sectors are now cached.
	if err := c.Read(ctx, 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := c.Read(ctx, 2*ss, buf2, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	if src.count() != 2 {
		t.Fatalf("backend reads = %d, want 2 (both sectors are cached)", src.count())
	}
}

func TestReadAcrossBlocks(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 4<<20)
	c := newTestCache(t, testOpts{})
	ctx := context.Background()

	// Span two blocks and start mid-sector.
	off := int64(bs - 2*ss)
	buf := make([]byte, 4*ss)
	if err := c.Read(ctx, off, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	expectPattern(t, ss, off, buf)
	// Both blocks are absent, so the request is one run and the backend is read
	// once; rounding to sectors only widens the run to the sector boundaries.
	if src.count() != 1 {
		t.Fatalf("backend reads = %d, want 1 (the run of absent blocks is one read)", src.count())
	}
}

// TestAbsentRunIsOneBackendRead pins the point of merging: a read spanning many
// absent blocks must cost one backend command, not one per block. A per block
// command carries the backing target's round trip per cache block, which is what
// caps a streamed read through the proxy.
func TestAbsentRunIsOneBackendRead(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 16<<20)
	c := newTestCache(t, testOpts{})
	ctx := context.Background()

	const blocks = 16
	buf := make([]byte, blocks*bs)
	if err := c.Read(ctx, 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	expectPattern(t, ss, 0, buf)
	if src.count() != 1 {
		t.Fatalf("backend reads = %d, want 1 for a %d block run", src.count(), blocks)
	}
	if st := c.Stats(); st.Fills != 1 || st.BackendReads != 1 {
		t.Fatalf("stats = %+v", st)
	}

	// The merged read populated every block, so the second read is all hits.
	if err := c.Read(ctx, 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	expectPattern(t, ss, 0, buf)
	if src.count() != 1 {
		t.Fatalf("backend reads = %d, want 1 (the run is cached)", src.count())
	}
	if st := c.Stats(); st.L1Hits != blocks || st.RequestHits != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestPartialFillThenRemainingSectors(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 4<<20)
	c := newTestCache(t, testOpts{})
	ctx := context.Background()

	// Fill only sector 0 of block 0 by invalidating the rest afterwards: use a
	// dedicated cache entry state by reading a single sector range directly.
	buf := make([]byte, ss)
	if err := c.Read(ctx, 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Wait for the fill so the block holds sector 0 before the rest is dropped.
	c.Wait()
	// Drop the last three sectors of the block.
	c.Invalidate(ss, 3*ss)

	buf = make([]byte, ss)
	if err := c.Read(ctx, 2*ss, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	expectPattern(t, ss, 2*ss, buf)
	if st := c.Stats(); st.PartialHits == 0 {
		t.Fatalf("expected a partial hit, stats = %+v", st)
	}
}

func TestInvalidateDropsEntry(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 4<<20)
	c := newTestCache(t, testOpts{})
	ctx := context.Background()

	buf := make([]byte, ss)
	if err := c.Read(ctx, 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	c.Invalidate(0, int64(bs))
	if st := c.Stats(); st.Invalidations != 1 {
		t.Fatalf("invalidations = %d", st.Invalidations)
	}

	if err := c.Read(ctx, 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	if src.count() != 2 {
		t.Fatalf("backend reads = %d, want 2 (the range must be re-read)", src.count())
	}
}

func TestInvalidateSpanningBlocks(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 4<<20)
	c := newTestCache(t, testOpts{})
	ctx := context.Background()

	buf := make([]byte, 2*bs)
	if err := c.Read(ctx, 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Invalidate a range that starts inside block 0 and ends inside block 1.
	c.Invalidate(int64(bs-ss), int64(2*ss))

	if err := c.Read(ctx, 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	// The first fill reads both absent blocks as one run. Only the invalidated
	// sectors then need refilling; each block still holds its other sectors, so
	// the second read costs one command per block.
	if src.count() != 3 {
		t.Fatalf("backend reads = %d, want 3", src.count())
	}
}

func TestScanDetectionBypassesCache(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 16<<20)
	c := newTestCache(t, testOpts{scan: true, threshold: 4})
	ctx := context.Background()

	buf := make([]byte, bs)
	for i := 0; i < 10; i++ {
		if err := c.Read(ctx, int64(i)*int64(bs), buf, src); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		expectPattern(t, ss, int64(i)*int64(bs), buf)
	}
	st := c.Stats()
	if st.ScanBypass != 7 {
		t.Fatalf("scan bypasses = %d, want 7", st.ScanBypass)
	}
	if st.Fills != 3 {
		t.Fatalf("fills = %d, want 3 (only the run before the threshold)", st.Fills)
	}
	if src.count() != 10 {
		t.Fatalf("backend reads = %d, want 10 (3 fills + 7 bypasses)", src.count())
	}
}

func TestScanDetectionDisabled(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 16<<20)
	c := newTestCache(t, testOpts{scan: false, threshold: 4})
	ctx := context.Background()

	buf := make([]byte, bs)
	for i := 0; i < 10; i++ {
		if err := c.Read(ctx, int64(i)*int64(bs), buf, src); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	c.Wait()
	if st := c.Stats(); st.ScanBypass != 0 || st.Fills != 10 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestDropEmptiesL1(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 4<<20)
	c := newTestCache(t, testOpts{})
	ctx := context.Background()

	buf := make([]byte, ss)
	if err := c.Read(ctx, 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	c.Drop()
	if err := c.Read(ctx, 0, buf, src); err != nil {
		t.Fatalf("read after drop: %v", err)
	}
	if src.count() != 2 {
		t.Fatalf("backend reads = %d, want 2", src.count())
	}
}

// TestConcurrentReadsSameBlockAreCorrect checks that concurrent readers of the
// same block each get correct data. Fills are asynchronous and are no longer
// deduplicated across readers (a pure miss bypasses the block path entirely), so
// the readers may each cost a backing read; what must hold is that none sees
// torn or inconsistent data and that the block is cached once the fills land.
func TestConcurrentReadsSameBlockAreCorrect(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	release := make(chan struct{})
	src := &blockingSource{fake: newFakeSource(ss, 4<<20), release: release}
	c := newTestCache(t, testOpts{})
	ctx := context.Background()

	const readers = 8
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, ss)
			if err := c.Read(ctx, 0, buf, src); err != nil {
				errs <- err
				return
			}
			expectPattern(t, ss, 0, buf)
		}()
	}
	// Let the readers pile up on the backend, then let the reads finish.
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read: %v", err)
	}
	if got := src.fake.count(); got < 1 || got > readers {
		t.Fatalf("backend reads = %d, want between 1 and %d", got, readers)
	}

	// Once the fills land, the block is cached and a re-read costs nothing.
	c.Wait()
	if err := c.Read(ctx, 0, make([]byte, ss), src); err != nil {
		t.Fatalf("read after fill: %v", err)
	}
	if got := src.fake.count(); got > readers {
		t.Fatalf("a read after the fills installed missed the cache: reads = %d", got)
	}
}

type blockingSource struct {
	fake    *fakeSource
	release chan struct{}
	// entered, when set, receives once as ReadAt is entered, so a test can order
	// an invalidation after the read has captured its epoch but before it has the
	// data back.
	entered chan struct{}
}

func (b *blockingSource) ReadAt(ctx context.Context, off int64, p []byte) error {
	if b.entered != nil {
		select {
		case b.entered <- struct{}{}:
		default:
		}
	}
	<-b.release
	return b.fake.ReadAt(ctx, off, p)
}

func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"block not a multiple of sector", Config{BlockSize: 10000, SectorSize: 4096, L1Bytes: 1 << 20, Shards: 4}},
		{"too many sectors per block", Config{BlockSize: 1 << 20, SectorSize: 4096, L1Bytes: 1 << 22, Shards: 4}},
		{"l1 too small", Config{BlockSize: 64 << 10, SectorSize: 4096, L1Bytes: 1 << 10, Shards: 8}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestReadExposesGeometry(t *testing.T) {
	c := newTestCache(t, testOpts{blockSize: 32 << 10, sectorSize: 4 << 10})
	if c.BlockSize() != 32<<10 || c.SectorSize() != 4<<10 {
		t.Fatalf("geometry = %d/%d", c.BlockSize(), c.SectorSize())
	}
}

func TestReadEmptyBuffer(t *testing.T) {
	src := newFakeSource(4<<10, 1<<20)
	c := newTestCache(t, testOpts{})
	if err := c.Read(context.Background(), 0, nil, src); err != nil {
		t.Fatalf("empty read: %v", err)
	}
	if src.count() != 0 {
		t.Fatal("an empty read must not touch the backend")
	}
}

// fakeL2 is an in-memory L2 whose writes can be held open, so a test can observe
// that an invalidation waits for a population already in flight.
type fakeL2 struct {
	mu      sync.Mutex
	secSize int
	sects   map[uint64]map[int][]byte
	// gate, when set, blocks every WriteSectors until it is closed.
	gate chan struct{}
	// entered, when set, receives once as WriteSectors is entered (before the
	// gate), so a test can order an invalidation after a population has been
	// enqueued in the fence.
	entered chan struct{}
}

func newFakeL2(secSize int) *fakeL2 {
	return &fakeL2{secSize: secSize, sects: map[uint64]map[int][]byte{}}
}

func (f *fakeL2) ReadSectors(blk uint64, sec, n int, dst []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.sects[blk]
	for i := 0; i < n; i++ {
		if m == nil || m[sec+i] == nil {
			return false, nil
		}
	}
	for i := 0; i < n; i++ {
		copy(dst[i*f.secSize:], m[sec+i])
	}
	return true, nil
}

func (f *fakeL2) WriteSectors(blk uint64, sec, n int, src []byte) error {
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.sects[blk]
	if m == nil {
		m = map[int][]byte{}
		f.sects[blk] = m
	}
	for i := 0; i < n; i++ {
		m[sec+i] = append([]byte(nil), src[i*f.secSize:(i+1)*f.secSize]...)
	}
	return nil
}

func (f *fakeL2) InvalidateSectors(blk uint64, sec, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 0; i < n; i++ {
		delete(f.sects[blk], sec+i)
	}
}

func (f *fakeL2) has(blk uint64, sec, n int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.sects[blk]
	for i := 0; i < n; i++ {
		if m == nil || m[sec+i] == nil {
			return false
		}
	}
	return true
}

// TestReadPopulatesL2InBackground pins that a miss still reaches L2: the write
// is asynchronous, so it is only guaranteed to have landed once Wait (which
// fences on the fill and its L2 population) has returned.
func TestReadPopulatesL2InBackground(t *testing.T) {
	const ss = 4 << 10
	src := newFakeSource(ss, 4<<20)
	l2 := newFakeL2(ss)
	c := newTestCache(t, testOpts{l2: l2})

	buf := make([]byte, ss)
	if err := c.Read(context.Background(), 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}
	c.Wait()
	if !l2.has(0, 0, 1) {
		t.Fatal("the read did not populate the L2")
	}
}

// TestInvalidateWaitsForBackgroundL2Write pins the ordering the asynchronous
// population depends on: an invalidation must not overtake a write already in
// flight, or the write would resurrect a sector that a write to the device has
// just made stale.
func TestInvalidateWaitsForBackgroundL2Write(t *testing.T) {
	const ss = 4 << 10
	src := newFakeSource(ss, 4<<20)
	gated := make(chan struct{})
	l2 := newFakeL2(ss)
	l2.gate = gated
	l2.entered = make(chan struct{}, 1)
	c := newTestCache(t, testOpts{l2: l2})

	buf := make([]byte, ss)
	if err := c.Read(context.Background(), 0, buf, src); err != nil {
		t.Fatalf("read: %v", err)
	}

	// Wait until the population has been handed to L2. Because the fill worker
	// enqueues it in the fence before starting the write, seeing it enter
	// WriteSectors guarantees the invalidation below has something to wait for.
	select {
	case <-l2.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the L2 population never started")
	}

	// The population is blocked in WriteSectors, so the invalidation must still
	// be waiting on it.
	done := make(chan struct{})
	go func() {
		c.Invalidate(0, ss)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Invalidate returned while an L2 write for the same range was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(gated)
	<-done

	if l2.has(0, 0, 1) {
		t.Fatal("the invalidated sector is still present in the L2")
	}
}

// TestAsyncFillDropsOnInvalidate pins the epoch rule that makes an asynchronous
// fill safe: a fill that captured its epoch before a write and read pre-write
// data must be dropped when the invalidation lands while it is in the backend.
func TestAsyncFillDropsOnInvalidate(t *testing.T) {
	const ss = 4 << 10
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	src := &blockingSource{fake: newFakeSource(ss, 4<<20), release: release, entered: entered}
	c := newTestCache(t, testOpts{})
	ctx := context.Background()

	// The read captures its epoch, then blocks in the backend before it has the
	// data back.
	errCh := make(chan error, 1)
	go func() { errCh <- c.Read(ctx, 0, make([]byte, ss), src) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the read never reached the backend")
	}

	// Invalidate while the read is in flight: its fill must be rejected.
	c.Invalidate(0, ss)
	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("read: %v", err)
	}
	c.Wait()

	// The dropped fill must not have published anything, so the block is still
	// cold and the next read goes back to the backend.
	if err := c.Read(ctx, 0, make([]byte, ss), src); err != nil {
		t.Fatalf("read after invalidate: %v", err)
	}
	if got := src.fake.count(); got != 2 {
		t.Fatalf("backend reads = %d, want 2 (the pre-invalidate fill was resurrected)", got)
	}
}

// TestInvalidatedFillDoesNotResurrectInL2 checks the same epoch rule for the L2
// population: a fill reading pre-invalidate data must not land in L2 after the
// invalidation has dropped the range.
func TestInvalidatedFillDoesNotResurrectInL2(t *testing.T) {
	const ss = 4 << 10
	l2 := newFakeL2(ss)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	src := &blockingSource{fake: newFakeSource(ss, 4<<20), release: release, entered: entered}
	c := newTestCache(t, testOpts{l2: l2})

	errCh := make(chan error, 1)
	go func() { errCh <- c.Read(context.Background(), 0, make([]byte, ss), src) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the read never reached the backend")
	}
	c.Invalidate(0, ss)
	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("read: %v", err)
	}
	c.Wait()

	if l2.has(0, 0, 1) {
		t.Fatal("a fill that read pre-invalidate data resurrected a sector in L2")
	}
}

// TestFillQueueDropKeepsServing pins that a bounded fill queue only ever drops a
// fill: with a depth-1 queue and one worker most fills are dropped, and the reads
// that produced them must still be served correctly.
func TestFillQueueDropKeepsServing(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 64<<20)
	c, err := New(Config{
		BlockSize: bs, SectorSize: ss, L1Bytes: 1 << 20, Shards: 4,
		FillWorkers: 1, FillQueueDepth: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	const n = 64
	for i := 0; i < n; i++ {
		off := int64(i) * bs
		buf := make([]byte, ss)
		if err := c.Read(ctx, off, buf, src); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		expectPattern(t, ss, off, buf)
	}
	// Distinct blocks never hit, so every read cost exactly one backend read: a
	// dropped fill never affected the read that produced it.
	if got := src.count(); got != n {
		t.Fatalf("backend reads = %d, want %d", got, n)
	}
	// Wait drains whatever survived the queue and returns; the cache stays usable.
	c.Wait()
	if err := c.Read(ctx, 0, make([]byte, ss), src); err != nil {
		t.Fatalf("read after Wait: %v", err)
	}
}
