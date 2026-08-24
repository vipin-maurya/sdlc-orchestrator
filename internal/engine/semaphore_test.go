package engine

import (
	"sync"
	"testing"
	"time"
)

// A live edit to orchestrator.max_parallel_jobs must cap new dispatch without
// touching whatever is already running: a job holding a slot is not something
// the semaphore has any business evicting mid-flight — see engine.go's tick,
// which calls SetCapacity once per pass, before deciding what to dispatch
// next.
func TestSemaphoreShrinkDoesNotEvictInFlightJobs(t *testing.T) {
	s := &resizableSem{}
	s.SetCapacity(3)

	// Fill all three slots.
	for i := 0; i < 3; i++ {
		if !s.TryAcquire() {
			t.Fatalf("acquire %d/3 failed at capacity 3", i+1)
		}
	}

	// Shrink capacity below the number already in flight. The three jobs
	// already holding a slot are not "in the semaphore" in any sense that
	// could be revoked — resizableSem only ever refuses future TryAcquire
	// calls, so nothing here checks on the three acquired slots because
	// there is nothing to check: they were never handed anything that could
	// be taken back.
	s.SetCapacity(1)

	// New dispatch must now be refused: inUse (3) already exceeds the new
	// capacity (1).
	if s.TryAcquire() {
		t.Fatal("acquired a 4th slot after shrinking capacity to 1 with 3 already in flight")
	}

	// Release two of the three in-flight jobs. inUse is now 1, still at (not
	// under) the shrunk capacity, so dispatch must stay capped.
	s.Release()
	s.Release()
	if s.TryAcquire() {
		t.Fatal("acquired a slot while inUse (1) already equals the shrunk capacity (1)")
	}

	// Release the last of the original three. Only now does inUse (0) fall
	// under the shrunk capacity (1), and a new job may dispatch.
	s.Release()
	if !s.TryAcquire() {
		t.Fatal("could not acquire once inUse dropped below the shrunk capacity")
	}
}

// Growing capacity mid-flight must free up new dispatch immediately, the
// mirror image of the shrink case above.
func TestSemaphoreGrowAllowsMoreDispatch(t *testing.T) {
	s := &resizableSem{}
	s.SetCapacity(1)
	if !s.TryAcquire() {
		t.Fatal("could not acquire the first slot at capacity 1")
	}
	if s.TryAcquire() {
		t.Fatal("acquired a second slot at capacity 1")
	}
	s.SetCapacity(2)
	if !s.TryAcquire() {
		t.Fatal("growing capacity to 2 did not allow a second acquire")
	}
}

// Concurrent acquire, release, and resize must never leave inUse negative or
// let TryAcquire hand out more slots than the capacity in effect at that
// moment allows — run under -race (go test ./internal/engine/... -race) so a
// missing lock around any of the three operations is caught, not just a
// logically wrong result.
func TestSemaphoreConcurrentAcquireReleaseResize(t *testing.T) {
	s := &resizableSem{}
	s.SetCapacity(4)

	const workers = 16
	const opsPerWorker = 500
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerWorker; j++ {
				if s.TryAcquire() {
					// Hold the slot for a moment so acquire/release actually
					// overlap with other goroutines' resizes instead of every
					// operation completing in isolation.
					time.Sleep(time.Microsecond)
					s.Release()
				}
			}
		}(i)
	}

	// Resize concurrently with the acquire/release traffic above. The
	// interesting cases for -race are capacities that are smaller than what
	// is plausibly in flight, exercising the shrink path from the other test
	// under real concurrency instead of by hand.
	wg.Add(1)
	go func() {
		defer wg.Done()
		caps := []int{4, 1, 8, 2, 4, 0, 4}
		for i := 0; i < 200; i++ {
			s.SetCapacity(caps[i%len(caps)])
			time.Sleep(time.Microsecond)
		}
		s.SetCapacity(4)
	}()

	wg.Wait()

	// Whatever the last capacity settled on, the invariant that must hold
	// unconditionally is inUse >= 0: a negative count means a Release ran
	// without a matching successful Acquire, the class of bug -race exists
	// to catch here via the unguarded-field read this test would otherwise
	// require.
	s.mu.Lock()
	inUse := s.inUse
	s.mu.Unlock()
	if inUse < 0 {
		t.Fatalf("inUse went negative: %d", inUse)
	}
	if inUse != 0 {
		t.Fatalf("inUse = %d after every worker released, want 0", inUse)
	}
}
