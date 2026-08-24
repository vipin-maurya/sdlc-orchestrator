package engine

import "sync"

// resizableSem is a counting semaphore whose capacity can change while
// acquires and releases are in flight — a fixed-size chan struct{} cannot be
// resized, and max_parallel_jobs is a runtime-configurable setting. tick
// calls SetCapacity once per pass from the live config before dispatching
// anything, so a config edit is visible to the next job considered within
// one tick; jobs already holding a slot when capacity shrinks keep running
// (see TestSemaphoreShrinkDoesNotEvictInFlightJobs) — only new acquisitions
// are capped.
type resizableSem struct {
	mu       sync.Mutex
	capacity int
	inUse    int
}

func (s *resizableSem) SetCapacity(n int) {
	s.mu.Lock()
	s.capacity = n
	s.mu.Unlock()
}

func (s *resizableSem) TryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inUse >= s.capacity {
		return false
	}
	s.inUse++
	return true
}

func (s *resizableSem) Release() {
	s.mu.Lock()
	s.inUse--
	s.mu.Unlock()
}
