package config

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// RestartRequiredFields names the keys in old that differ from new among the
// settings a running process cannot pick up by re-reading the file: each one
// is already baked into a resource the process opened before Live existed to
// ask — a bound listener, an open database connection, a lock file another
// process is coordinating against. Changing DataDir live would not move the
// job directories any in-flight job already has open; changing Database.Path
// or BusyTimeout would not reopen the connection sql.Open already made;
// changing LockFile would leave the process holding a lock at a path it no
// longer reports; changing Server.Listen cannot rebind a socket from inside
// the handler running on it.
//
// Everything else in Config is read fresh at the point of use — a target's
// build command, a limit, a policy, an agent's model — so a change there is
// live the moment Reload or Apply swaps the pointer, with no entry here.
//
// The returned names are dotted config keys, sorted, for a caller to print;
// order carries no other meaning. An empty result means new is safe to swap
// in without restarting anything.
func RestartRequiredFields(old, new *Config) []string {
	var out []string
	if old.Orchestrator.DataDir != new.Orchestrator.DataDir {
		out = append(out, "orchestrator.data_dir")
	}
	if old.Orchestrator.LockFile != new.Orchestrator.LockFile {
		out = append(out, "orchestrator.lock_file")
	}
	if old.Database != new.Database {
		out = append(out, "database")
	}
	if old.Server.Listen != new.Server.Listen {
		out = append(out, "server.listen")
	}
	sort.Strings(out)
	return out
}

// Live is a Config that can change while the process holding it keeps
// running. It exists only for the two commands that run unattended for a
// long time — `sdlc run` and `sdlc serve` — and read the same file: every
// one-shot command (submit, approve, status, validate, ...) does one thing
// and exits, and keeps using the plain *Config Load already returns.
//
// A zero Live is not usable; construct one with NewLive.
type Live struct {
	p    atomic.Pointer[Config]
	path string

	// mu serializes Reload and Apply against each other and against
	// themselves. Reloads are rare, human-timescale events — a file changing
	// on disk, an operator clicking save — so correctness by serializing
	// costs nothing worth avoiding; what it buys is that two reloads racing
	// (a poll tick and a POST landing at the same moment) cannot interleave
	// their stat/read/backup/write/swap steps.
	mu sync.Mutex
	// modTime and size are the poll optimization: Watch skips the read and
	// reparse entirely when neither has moved since the last look, so a file
	// nobody is touching costs one Stat per tick, not one full Load.
	modTime time.Time
	size    int64
}

// NewLive wraps a *Config already produced by Load or Parse. It trusts the
// caller that initial was validated — NewLive does not re-validate it — so
// that the same startup error Load would have returned still happens before
// Live exists, with the same message, rather than being reported one level
// removed.
func NewLive(initial *Config) *Live {
	l := &Live{path: initial.Path}
	l.p.Store(initial)
	if fi, err := os.Stat(initial.Path); err == nil {
		l.modTime, l.size = fi.ModTime(), fi.Size()
	}
	return l
}

// Get returns the config currently in effect. It never blocks and never
// returns nil; the pointer swap in Reload/Apply is what makes this safe to
// call from any number of goroutines while a reload is in flight elsewhere —
// a caller either sees the config before the swap or the one after it, never
// a half-applied one.
func (l *Live) Get() *Config { return l.p.Load() }

// ReloadResult reports what Reload or Apply did, so a caller can log it or
// show it rather than Live deciding that on their behalf. Exactly one of
// Applied, Unchanged, or Refused-with-a-reason (RestartFields non-empty, or
// Err non-nil) is true of any result.
type ReloadResult struct {
	// Applied is true when the new config is now what Get returns.
	Applied bool
	// RestartFields is non-empty when the file changed but the change was
	// refused because it touched a setting only a restart can pick up. The
	// config in effect is unchanged; Applied is false.
	RestartFields []string
	// Err is set when the file changed but failed to parse or validate. The
	// config in effect is unchanged; Applied is false. It is never a "the
	// file could not be read" error from a Watch tick that found nothing to
	// do — that case is silent, which is what makes Watch cheap to run every
	// couple of seconds against a file nobody is editing.
	Err error
}

func (r ReloadResult) String() string {
	switch {
	case r.Applied:
		return "applied"
	case len(r.RestartFields) > 0:
		return fmt.Sprintf("refused (restart required for: %s)", strJoin(r.RestartFields))
	case r.Err != nil:
		return fmt.Sprintf("refused: %v", r.Err)
	default:
		return "unchanged"
	}
}

func strJoin(ss []string) string {
	out := ss[0]
	for _, s := range ss[1:] {
		out += ", " + s
	}
	return out
}

// Reload re-reads and re-parses the file at l's path unconditionally — unlike
// Watch's poll loop, it does not check whether the file moved first, so a
// caller who already knows it changed (or wants to force a re-check) is not
// second-guessed. A parse or validation failure, or a change that touches a
// RestartRequiredFields entry, leaves Get() returning what it returned
// before: a bad edit degrades to "the edit did not take," never to "the
// process is now running on something that failed to validate."
func (l *Live) Reload() ReloadResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return ReloadResult{Err: fmt.Errorf("read %s: %w", l.path, err)}
	}
	next, rf, err := l.checkLocked(raw)
	if err != nil || len(rf) > 0 {
		return ReloadResult{RestartFields: rf, Err: err}
	}
	l.swapLocked(next)
	return ReloadResult{Applied: true}
}

// Apply validates raw as a complete replacement, and only if that succeeds
// and touches nothing in RestartRequiredFields does it write raw to l's path
// — backing up what was there first — and swap it in. A save that fails
// validation, or that would require a restart, never reaches the disk: the
// file on disk is only ever overwritten by content already proven equivalent
// to what Load would accept.
func (l *Live) Apply(raw []byte) ReloadResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	next, rf, err := l.checkLocked(raw)
	if err != nil || len(rf) > 0 {
		return ReloadResult{RestartFields: rf, Err: err}
	}
	if err := writeWithBackup(l.path, raw); err != nil {
		return ReloadResult{Err: fmt.Errorf("write %s: %w", l.path, err)}
	}
	l.swapLocked(next)
	return ReloadResult{Applied: true}
}

// checkLocked is the half Reload and Apply share: parse raw and diff it
// against what is currently live. Neither caller has written or swapped
// anything yet when this returns, which is what lets Apply decide whether raw
// is fit to reach disk at all before it does.
func (l *Live) checkLocked(raw []byte) (next *Config, restartFields []string, err error) {
	next, err = Parse(raw, l.path)
	if err != nil {
		return nil, nil, err
	}
	return next, RestartRequiredFields(l.p.Load(), next), nil
}

// swapLocked makes next what Get returns and refreshes the poll baseline so
// Watch's next tick does not mistake this call's own write for a further
// external change.
func (l *Live) swapLocked(next *Config) {
	l.p.Store(next)
	if fi, err := os.Stat(l.path); err == nil {
		l.modTime, l.size = fi.ModTime(), fi.Size()
	}
}

// Watch polls the file every interval until ctx is done, calling Reload only
// when its mtime or size has moved since the last look — a file nobody is
// editing costs one os.Stat per tick, not one full parse — and logs the
// outcome through lg whenever Reload actually ran. It is meant to be started
// once, in a goroutine, by the command that owns this Live; Get is safe to
// call from anywhere else in the meantime.
func (l *Live) Watch(ctx context.Context, interval time.Duration, lg *log.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.pollOnce(lg)
		}
	}
}

func (l *Live) pollOnce(lg *log.Logger) {
	fi, err := os.Stat(l.path)
	if err != nil {
		// A file that briefly vanishes mid-edit (many editors write via a
		// temp file and rename) is not news; the next tick will see it again
		// or, if it is really gone, every tick will report the same thing and
		// the operator already knows because nothing else works either.
		return
	}
	l.mu.Lock()
	moved := !fi.ModTime().Equal(l.modTime) || fi.Size() != l.size
	l.mu.Unlock()
	if !moved {
		return
	}
	res := l.Reload()
	switch {
	case res.Applied:
		lg.Printf("config: reloaded %s", l.path)
	case len(res.RestartFields) > 0:
		lg.Printf("config: %s changed %s, which needs a restart to take effect; still running on the previous values",
			l.path, strJoin(res.RestartFields))
	case res.Err != nil:
		lg.Printf("config: %s changed but did not validate, still running on the previous version: %v", l.path, res.Err)
	}
}

// writeWithBackup copies the file currently at path into a sibling
// .sdlc-config-history directory before overwriting it, then writes new via a
// temp file and rename so a reader never observes a partial file — the same
// reason review.Write and the diff artifacts in this codebase write that way.
// It prunes the history directory to the newest maxHistoryFiles afterward,
// so an operator who saves often does not grow it without bound.
func writeWithBackup(path string, next []byte) error {
	dir := filepath.Dir(path)
	if cur, err := os.ReadFile(path); err == nil {
		histDir := filepath.Join(dir, ".sdlc-config-history")
		if err := os.MkdirAll(histDir, 0o755); err != nil {
			return err
		}
		stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
		if err := os.WriteFile(filepath.Join(histDir, stamp+"-"+filepath.Base(path)), cur, 0o644); err != nil {
			return err
		}
		prune(histDir, maxHistoryFiles)
	}
	tmp, err := os.CreateTemp(dir, ".sdlc-config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(next); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// maxHistoryFiles bounds .sdlc-config-history: an unattended engine that
// nobody edits the config of for months should not be the reason a save from
// the UI, months later, is slow because prune has years of files to sort.
const maxHistoryFiles = 50

func prune(dir string, keep int) {
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) <= keep {
		return
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	// Filenames are a UTC timestamp prefix, so lexical order is chronological
	// order; no need to stat each one for its mtime.
	sort.Strings(names)
	for _, n := range names[:max(0, len(names)-keep)] {
		os.Remove(filepath.Join(dir, n))
	}
}
