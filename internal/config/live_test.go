package config

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeLoad(t *testing.T, body string) (*Live, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sdlc.yaml")
	if err := os.WriteFile(p, []byte(minimalTarget+body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return NewLive(cfg), p
}

func TestRestartRequiredFieldsNamesExactlyTheBakedInSettings(t *testing.T) {
	base, err := Parse([]byte(minimalTarget), "/x/sdlc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   []string
	}{
		{"data_dir", func(c *Config) { c.Orchestrator.DataDir = "/elsewhere" }, []string{"orchestrator.data_dir"}},
		{"lock_file", func(c *Config) { c.Orchestrator.LockFile = "/elsewhere.lock" }, []string{"orchestrator.lock_file"}},
		{"database.path", func(c *Config) { c.Database.Path = "/elsewhere.db" }, []string{"database"}},
		{"database.busy_timeout", func(c *Config) { c.Database.BusyTimeout = Duration(9 * time.Second) }, []string{"database"}},
		{"server.listen", func(c *Config) { c.Server.Listen = "127.0.0.1:9" }, []string{"server.listen"}},
		{"two at once", func(c *Config) {
			c.Server.Listen = "127.0.0.1:9"
			c.Orchestrator.DataDir = "/elsewhere"
		}, []string{"orchestrator.data_dir", "server.listen"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := *base
			tc.mutate(&next)
			got := RestartRequiredFields(base, &next)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRestartRequiredFieldsIsSilentOnEverythingElse is the other half of the
// same claim: RestartRequiredFields' doc says everything not named is live.
// A limit, a target's build command, and a policy are exercised here as
// representative live-safe fields; if any of them started appearing in the
// result it would mean a change here silently downgraded something the
// doc — and the UI's save flow — promises applies without a restart.
func TestRestartRequiredFieldsIsSilentOnEverythingElse(t *testing.T) {
	base, err := Parse([]byte(minimalTarget), "/x/sdlc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	next := *base
	next.Orchestrator.MaxParallelJobs = base.Orchestrator.MaxParallelJobs + 3
	next.Orchestrator.PollInterval = Duration(90 * time.Second)
	next.Limits.MaxJobDuration = Duration(2 * time.Hour)
	t2 := next.Targets["demo"]
	t2.BranchPrefix = "changed/"
	next.Targets = map[string]Target{"demo": t2}
	if got := RestartRequiredFields(base, &next); len(got) != 0 {
		t.Errorf("live-safe fields reported as restart-required: %v", got)
	}
}

func TestLiveApplyIsVisibleImmediately(t *testing.T) {
	live, path := writeLoad(t, "")
	if got := live.Get().Orchestrator.MaxParallelJobs; got != Default().Orchestrator.MaxParallelJobs {
		t.Fatalf("starting value = %d", got)
	}
	edited := strings.Replace(mustRead(t, path), "targets:", "orchestrator:\n  max_parallel_jobs: 9\ntargets:", 1)
	res := live.Apply([]byte(edited))
	if !res.Applied {
		t.Fatalf("Apply refused a valid edit: %+v", res)
	}
	if got := live.Get().Orchestrator.MaxParallelJobs; got != 9 {
		t.Errorf("Get() after Apply = %d, want 9", got)
	}
}

// TestLiveApplyRefusesInvalidTextWithoutTouchingDisk is the property the
// whole write path exists to guarantee: sdlc.yaml is never overwritten with
// something Load would have rejected.
func TestLiveApplyRefusesInvalidTextWithoutTouchingDisk(t *testing.T) {
	live, path := writeLoad(t, "")
	before := mustRead(t, path)
	res := live.Apply([]byte("not: valid: yaml: at: all: :::"))
	if res.Applied || res.Err == nil {
		t.Fatalf("an invalid edit was accepted: %+v", res)
	}
	if got := mustRead(t, path); got != before {
		t.Error("the file on disk changed despite the edit being invalid")
	}
	if got := live.Get().Orchestrator.MaxParallelJobs; got != Default().Orchestrator.MaxParallelJobs {
		t.Error("the live config changed despite the edit being invalid")
	}
}

// TestLiveApplyRefusesARestartOnlyChangeWithoutTouchingDisk mirrors the
// invalid-text case for the other refusal reason: even syntactically valid,
// fully-Validate()-passing content is refused, and left off disk, if it
// touches a RestartRequiredFields setting — because Apply cannot make that
// change take effect, and writing it would leave the file claiming a listen
// address the running process is not actually bound to.
func TestLiveApplyRefusesARestartOnlyChangeWithoutTouchingDisk(t *testing.T) {
	live, path := writeLoad(t, "")
	before := mustRead(t, path)
	edited := strings.Replace(before, "targets:", "server:\n  listen: 127.0.0.1:9999\ntargets:", 1)
	res := live.Apply([]byte(edited))
	if res.Applied {
		t.Fatal("a restart-only change was applied")
	}
	if strings.Join(res.RestartFields, ",") != "server.listen" {
		t.Errorf("RestartFields = %v, want [server.listen]", res.RestartFields)
	}
	if got := mustRead(t, path); got != before {
		t.Error("the file on disk changed despite the edit needing a restart")
	}
}

// TestLiveApplyBacksUpTheReplacedVersion holds the point of writeWithBackup:
// a save is not a way to lose the version that came before it.
func TestLiveApplyBacksUpTheReplacedVersion(t *testing.T) {
	live, path := writeLoad(t, "")
	before := mustRead(t, path)
	edited := strings.Replace(before, "targets:", "orchestrator:\n  max_parallel_jobs: 4\ntargets:", 1)
	if res := live.Apply([]byte(edited)); !res.Applied {
		t.Fatalf("refused: %+v", res)
	}
	hist := filepath.Join(filepath.Dir(path), ".sdlc-config-history")
	ents, err := os.ReadDir(hist)
	if err != nil {
		t.Fatalf("no history directory: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("got %d history files, want 1", len(ents))
	}
	got := mustRead(t, filepath.Join(hist, ents[0].Name()))
	if got != before {
		t.Error("the backed-up file is not byte-identical to the version it replaced")
	}
}

func TestPruneKeepsOnlyTheNewest(t *testing.T) {
	live, path := writeLoad(t, "")
	for i := 1; i <= maxHistoryFiles+5; i++ {
		body := "orchestrator:\n  max_parallel_jobs: " + strconv.Itoa(i) + "\n" + minimalTarget
		if res := live.Apply([]byte(body)); !res.Applied {
			t.Fatalf("iteration %d refused: %+v", i, res)
		}
	}
	hist := filepath.Join(filepath.Dir(path), ".sdlc-config-history")
	ents, err := os.ReadDir(hist)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != maxHistoryFiles {
		t.Errorf("history has %d files, want exactly the cap of %d", len(ents), maxHistoryFiles)
	}
}

// TestLiveReloadPicksUpAnExternalEdit is the case Apply cannot cover: a
// second process (or a person with a text editor) changes the file directly.
// Live has to notice that too, since "runtime configurable" is a property of
// the file, not of one HTTP handler.
func TestLiveReloadPicksUpAnExternalEdit(t *testing.T) {
	live, path := writeLoad(t, "")
	edited := strings.Replace(mustRead(t, path), "targets:", "orchestrator:\n  max_parallel_jobs: 7\ntargets:", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	res := live.Reload()
	if !res.Applied {
		t.Fatalf("Reload did not pick up the external edit: %+v", res)
	}
	if got := live.Get().Orchestrator.MaxParallelJobs; got != 7 {
		t.Errorf("Get() = %d, want 7", got)
	}
}

func TestLiveWatchAppliesAndStopsOnCancel(t *testing.T) {
	live, path := writeLoad(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	lg := log.New(new(discard), "", 0)
	go func() { live.Watch(ctx, 10*time.Millisecond, lg); close(done) }()

	edited := strings.Replace(mustRead(t, path), "targets:", "orchestrator:\n  max_parallel_jobs: 6\ntargets:", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for live.Get().Orchestrator.MaxParallelJobs != 6 {
		select {
		case <-deadline:
			t.Fatal("Watch never picked up the external edit")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after its context was cancelled")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// TestLiveConcurrentApplyIsRace tries to make Reload/Apply/Get interleave
// badly, under -race. It asserts only that nothing races and Get() never
// observes a torn value — not any particular winner among concurrent writers,
// since which of several simultaneous valid saves lands last is not a
// contract this type makes.
func TestLiveConcurrentApplyIsRace(t *testing.T) {
	live, path := writeLoad(t, "")
	base := mustRead(t, path)
	var writers, readers sync.WaitGroup
	stop := make(chan struct{})

	// Readers, hammering Get() until told to stop. Started before the
	// writers and stopped after them, so they are live for the writers'
	// entire run rather than racing to start or finish alongside them.
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if got := live.Get().Orchestrator.MaxParallelJobs; got < 1 {
						t.Errorf("torn read: MaxParallelJobs = %d", got)
					}
				}
			}
		}()
	}
	// A watcher reloading concurrently with the writers below.
	ctx, cancel := context.WithCancel(context.Background())
	go live.Watch(ctx, time.Millisecond, log.New(new(discard), "", 0))

	// Writers, each posting a distinct valid value through Apply. Waited on
	// separately from the readers: the readers' exit depends on stop, which
	// must not close until every writer that might still be racing with a
	// reader has finished, or stop closing and Apply's mutex contention could
	// interleave in ways this test is not trying to assert about.
	for i := 1; i <= 20; i++ {
		writers.Add(1)
		go func(n int) {
			defer writers.Done()
			edited := strings.Replace(base, "targets:",
				"orchestrator:\n  max_parallel_jobs: "+strconv.Itoa(n)+"\ntargets:", 1)
			live.Apply([]byte(edited))
		}(i)
	}
	writers.Wait()

	cancel()
	close(stop)
	readers.Wait()
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
