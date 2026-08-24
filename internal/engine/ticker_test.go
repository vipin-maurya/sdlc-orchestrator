package engine

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// fakeTicker is a tickerSrc a test drives by hand: sending on c stands in for
// a real tick firing, and every Reset call is recorded (and, for tests that
// need to know exactly when one happened rather than polling for it,
// broadcast on resetCh) so a live poll-interval change can be proven without
// depending on real wall-clock timing anywhere.
type fakeTicker struct {
	c       chan time.Time
	resetCh chan time.Duration

	mu      sync.Mutex
	resets  []time.Duration
	stopped bool
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{c: make(chan time.Time, 1), resetCh: make(chan time.Duration, 8)}
}

func (f *fakeTicker) C() <-chan time.Time { return f.c }

func (f *fakeTicker) Reset(d time.Duration) {
	f.mu.Lock()
	f.resets = append(f.resets, d)
	f.mu.Unlock()
	select {
	case f.resetCh <- d:
	default:
	}
}

func (f *fakeTicker) Stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
}

func (f *fakeTicker) resetCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resets)
}

// --- unit test of the exact decision Run's loop makes every iteration -----

// maybeResetPollTicker is the whole of the interval-change decision, pulled
// out of Run precisely so it can be checked directly against a fake ticker:
// no engine run, no store, no real ticks, just "does it call Reset exactly
// when the live value moved, with the right value, and not otherwise."
func TestMaybeResetPollTickerOnlyResetsOnChange(t *testing.T) {
	cfg, err := config.Parse([]byte("orchestrator:\n  poll_interval: 3s\n"), filepath.Join(t.TempDir(), "sdlc.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	live := config.NewLive(cfg)
	e := &Engine{cfg: live}
	ft := newFakeTicker()

	current := 3 * time.Second
	current = e.maybeResetPollTicker(ft, current)
	if current != 3*time.Second {
		t.Fatalf("current = %s, want 3s", current)
	}
	if n := ft.resetCount(); n != 0 {
		t.Fatalf("Reset called %d time(s) with no config change, want 0", n)
	}

	// Repeat with the same value: still no reset, the whole point of tracking
	// "current" rather than resetting unconditionally on every tick.
	current = e.maybeResetPollTicker(ft, current)
	if n := ft.resetCount(); n != 0 {
		t.Fatalf("Reset called %d time(s) on a second no-op check, want 0", n)
	}

	// Apply a real live edit — the same atomic swap a file watch or the
	// server's /config POST would trigger — and confirm the next check picks
	// it up.
	res := live.Apply([]byte("orchestrator:\n  poll_interval: 750ms\n"))
	if !res.Applied {
		t.Fatalf("Apply refused: %s", res)
	}
	current = e.maybeResetPollTicker(ft, current)
	if current != 750*time.Millisecond {
		t.Fatalf("current = %s, want 750ms", current)
	}
	if n := ft.resetCount(); n != 1 {
		t.Fatalf("Reset called %d time(s) after the interval changed, want 1", n)
	}
	if ft.resets[0] != 750*time.Millisecond {
		t.Fatalf("Reset called with %s, want 750ms", ft.resets[0])
	}
}

// --- integration test: the real Run loop, not just the extracted helper ---

// TestRunPicksUpLiveIntervalChangeMidRun proves the wiring, not just the
// logic: it runs Engine.Run itself (the real loop, real select on
// ticker.C()), with the real *config.Live a CLI-owned Watch goroutine would
// hand the engine in production, and asserts that changing
// orchestrator.poll_interval mid-run makes Run call ticker.Reset with the new
// value — without ever waiting on a real wall-clock tick, via the newTicker
// injection seam.
func TestRunPicksUpLiveIntervalChangeMidRun(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Parse([]byte("orchestrator:\n  poll_interval: 3s\n"), filepath.Join(dir, "sdlc.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Database.Path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	live := config.NewLive(cfg)

	buf := &syncBuf{}
	eng := New(live, st, log.New(buf, "", 0))

	// Swap in a fake ticker factory for the duration of this test, and learn
	// about the one Run constructs via a channel rather than a shared
	// variable read from two goroutines.
	built := make(chan *fakeTicker, 1)
	orig := newTicker
	newTicker = func(d time.Duration) tickerSrc {
		ft := newFakeTicker()
		select {
		case built <- ft:
		default:
		}
		return ft
	}
	t.Cleanup(func() { newTicker = orig })

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- eng.Run(ctx, false) }()

	var ft *fakeTicker
	select {
	case ft = <-built:
	case <-time.After(10 * time.Second):
		t.Fatal("Run never constructed a ticker")
	}

	// Apply the live edit before letting the loop go around again: the next
	// iteration's tick()+maybeResetPollTicker must observe it.
	res := live.Apply([]byte("orchestrator:\n  poll_interval: 750ms\n"))
	if !res.Applied {
		cancel()
		t.Fatalf("Apply refused: %s", res)
	}

	// Unblock Run's current select so it loops back to tick() and the reset
	// check above.
	select {
	case ft.c <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("could not feed the fake ticker")
	}

	select {
	case got := <-ft.resetCh:
		if got != 750*time.Millisecond {
			t.Errorf("ticker reset to %s, want 750ms", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run never called ticker.Reset after the live interval changed")
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not exit after ctx cancellation")
	}

	// Belt and suspenders: acquireLock's lock file must exist under the
	// config's data dir, confirming Run actually started rather than the
	// whole thing being a no-op that happened to satisfy the channel reads
	// above by accident.
	if _, err := os.Stat(filepath.Dir(cfg.Orchestrator.LockFile)); err != nil {
		t.Errorf("engine data dir missing, Run may not have started: %v", err)
	}
}
