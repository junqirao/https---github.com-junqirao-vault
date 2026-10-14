package l2

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errNoOverlap = errors.New("the second read never got in flight")

// gateStore holds every store transfer open until a second one has arrived. It
// is how a test can observe that two reads really are allowed to be in flight at
// once rather than merely hoping they interleave.
type gateStore struct {
	Store
	armed   atomic.Bool
	once    sync.Once
	mu      sync.Mutex
	arrived int
	want    int
	release chan struct{}
}

func newGateStore(inner Store, want int) *gateStore {
	return &gateStore{Store: inner, want: want, release: make(chan struct{})}
}

func (g *gateStore) ReadAt(p []byte, off int64) error {
	if g.armed.Load() {
		g.mu.Lock()
		g.arrived++
		reached := g.arrived >= g.want
		g.mu.Unlock()
		if reached {
			g.once.Do(func() { close(g.release) })
		}
		select {
		case <-g.release:
		case <-time.After(5 * time.Second):
			return errNoOverlap
		}
	}
	return g.Store.ReadAt(p, off)
}

// TestReadsOfDifferentBlocksOverlap pins down the reason the index lock is taken
// for read: while one block's transfer is in flight, a read of another block has
// to be able to start its own. Held exclusively (as it was), this test fails on
// the timeout, because the second read cannot even reach the store.
func TestReadsOfDifferentBlocksOverlap(t *testing.T) {
	inner := NewMemoryStore(testSize)
	f := openTestL2(t, inner, "ns-overlap")
	defer f.Close()
	for blk := uint64(0); blk < 2; blk++ {
		if err := f.WriteSectors(blk, 0, 1, pattern(testSector, byte(blk+1))); err != nil {
			t.Fatalf("write block %d: %v", blk, err)
		}
	}

	gate := newGateStore(inner, 2)
	f.store = gate
	gate.armed.Store(true)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for blk := uint64(0); blk < 2; blk++ {
		wg.Add(1)
		go func(blk uint64) {
			defer wg.Done()
			dst := make([]byte, testSector)
			ok, err := f.ReadSectors(blk, 0, 1, dst)
			if err != nil {
				errs <- fmt.Errorf("read block %d: %w", blk, err)
				return
			}
			if !ok {
				errs <- fmt.Errorf("read block %d missed a slot it had just filled", blk)
				return
			}
			if want := pattern(testSector, byte(blk+1)); !bytes.Equal(dst, want) {
				errs <- fmt.Errorf("read block %d returned the wrong bytes", blk)
			}
		}(blk)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestReadsAndRecycleDoNotRace checks the other half of the same decision: a
// reader holding the lock must stop the ring from handing its slot to another
// block, so a read can never return a slot that has moved on underneath it.
func TestReadsAndRecycleDoNotRace(t *testing.T) {
	inner := NewMemoryStore(testSize)
	f := openTestL2(t, inner, "ns-recycle")
	defer f.Close()
	// Fill every slot so the next write has to recycle one.
	for blk := uint64(0); blk < uint64(f.Slots()); blk++ {
		if err := f.WriteSectors(blk, 0, 1, pattern(testSector, byte(blk+1))); err != nil {
			t.Fatalf("write block %d: %v", blk, err)
		}
	}
	gate := newGateStore(inner, 1)
	f.store = gate
	gate.armed.Store(true)

	done := make(chan error, 1)
	go func() {
		for blk := uint64(0); blk < uint64(f.Slots()); blk++ {
			dst := make([]byte, testSector)
			ok, err := f.ReadSectors(blk, 0, 1, dst)
			if err != nil {
				done <- err
				return
			}
			if ok && !bytes.Equal(dst, pattern(testSector, byte(blk+1))) {
				done <- fmt.Errorf("block %d came back with another block's bytes", blk)
				return
			}
		}
		done <- nil
	}()
	// Recycle while those reads are in flight.
	for blk := uint64(0); blk < uint64(f.Slots())+1; blk++ {
		fresh := uint64(100 + blk)
		if err := f.WriteSectors(fresh, 0, 1, pattern(testSector, byte(fresh))); err != nil {
			t.Fatalf("recycling write %d: %v", fresh, err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
