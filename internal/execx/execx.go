// Package execx runs external commands with streaming log capture, timeouts,
// env overlays, and Windows .bat/.cmd handling (SPEC §8). No shell is ever
// involved except the cmd /c wrapper required for batch files.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Cmd struct {
	Argv    []string
	Dir     string
	Env     map[string]string // overlaid on os.Environ()
	Timeout time.Duration     // 0 = no timeout
	LogPath string            // "" = discard; stdout+stderr interleaved
	Stdin   string            // literal stdin content ("" = none)
	// StderrSeparate keeps stderr out of RunCapture's returned string on
	// success, so callers that PARSE stdout are not fed diagnostics. Set it
	// for anything machine-read (git plumbing); leave it off for logs, where
	// interleaving is what you want. On a non-zero exit stderr is appended
	// anyway, so error messages keep their detail. Ignored by Run.
	StderrSeparate bool
	// OnLine, when set, is called with each complete output line as the
	// command produces it, before the command exits. It is what makes a long
	// agent run observable: without it a 13-minute state is a blank terminal
	// and a hung one looks identical. Called from the process's reader
	// goroutine, so it must not block for long.
	OnLine func(string)
}

type Result struct {
	ExitCode int
	Duration time.Duration
	TimedOut bool
	LogPath  string
	// Truncated reports that maxBytes stopped the returned string short of the
	// command's real output. limitedWriter reports a full write after it has
	// stopped copying — it has to, or os/exec would tear the command down with
	// a short-write error — so without this flag a caller cannot distinguish a
	// 4 MiB patch from the head of a 40 MiB one. A reviewer approving a merge
	// from a patch that was silently clipped is deciding on a change they have
	// not seen.
	Truncated bool
}

// Run executes the command. A non-zero exit is NOT a Go error; err is
// reserved for spawn/plumbing failures. Run captures nothing and so caps
// nothing: its Result.Truncated is always false.
func Run(ctx context.Context, c Cmd) (Result, error) {
	if len(c.Argv) == 0 {
		return Result{}, fmt.Errorf("empty argv")
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	argv := adaptWindows(c.Argv, c.Dir)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = overlayEnv(c.Env)
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}

	var sink io.Writer = io.Discard
	if c.LogPath != "" {
		f, err := createLog(c.LogPath)
		if err != nil {
			return Result{}, err
		}
		defer f.Close()
		sink = f
	}
	if c.OnLine != nil {
		sink = io.MultiWriter(sink, newLineWriter(c.OnLine))
	}
	cmd.Stdout = sink
	cmd.Stderr = sink

	start := time.Now()
	err := cmd.Run()
	res := Result{Duration: time.Since(start), LogPath: c.LogPath}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, err
	}
	res.ExitCode = 0
	return res, nil
}

// RunCapture is Run but additionally returns combined output as a string
// (bounded to maxBytes; 0 = unbounded). Used for small helper commands
// (git, adb) — big builds should stream to a log file instead.
//
// When LogPath is set the log is written *as the command runs*, not after it
// exits. The distinction matters for agents: an unattended run's only window
// into a state that has been going for ten minutes is `sdlc logs --last`, and
// a log file that materialises only on exit is no window at all.
func RunCapture(ctx context.Context, c Cmd, maxBytes int64) (Result, string, error) {
	if len(c.Argv) == 0 {
		return Result{}, "", fmt.Errorf("empty argv")
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	argv := adaptWindows(c.Argv, c.Dir)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = overlayEnv(c.Env)
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	var sb strings.Builder
	var w io.Writer = &sb
	// The limiter is held as a concrete *limitedWriter, not just as the
	// io.Writer it is wrapped into below, because whether it dropped anything
	// is only readable off the struct — once it is inside a MultiWriter there
	// is nothing left to ask.
	var lw *limitedWriter
	if maxBytes > 0 {
		lw = &limitedWriter{w: &sb, n: maxBytes}
		w = lw
	}
	// The captured string is bounded; the log file is not. A truncated capture
	// must not truncate the log an operator reads afterwards, so the two are
	// separate sinks rather than one written from the other.
	var extra []io.Writer
	if c.LogPath != "" {
		f, err := createLog(c.LogPath)
		if err != nil {
			return Result{}, "", err
		}
		defer f.Close()
		extra = append(extra, f)
	}
	if c.OnLine != nil {
		extra = append(extra, newLineWriter(c.OnLine))
	}
	// With StderrSeparate the two streams are distinct writers, so os/exec
	// pumps them from two goroutines — and they share these sinks. The mutex
	// is what keeps the line splitter's buffer and the log file from being
	// written concurrently.
	var side io.Writer
	if len(extra) > 0 {
		side = &syncWriter{w: io.MultiWriter(extra...)}
		w = io.MultiWriter(w, side)
	}
	cmd.Stdout = w
	var errb strings.Builder
	var elw *limitedWriter
	if c.StderrSeparate {
		// Diagnostics stay out of the parsed string but still reach the log:
		// a backend that fails with everything on stderr must not produce an
		// empty log file.
		elw = &limitedWriter{w: &errb, n: 1 << 16}
		var ew io.Writer = elw
		if side != nil {
			ew = io.MultiWriter(ew, side)
		}
		cmd.Stderr = ew
	} else {
		cmd.Stderr = w
	}
	start := time.Now()
	err := cmd.Run()
	// Both limiters count: on the error path withErr appends the stderr
	// capture to the string the caller gets back, so bytes dropped from either
	// one are bytes missing from that string.
	res := Result{Duration: time.Since(start), Truncated: lw.dropped() || elw.dropped()}
	out := sb.String()
	// Failed commands get their stderr back: the caller is reporting, not
	// parsing. Successful ones keep stdout clean.
	withErr := func() string {
		if !c.StderrSeparate || errb.Len() == 0 {
			return out
		}
		return out + errb.String()
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res, withErr(), nil
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, withErr(), nil
		}
		return res, withErr(), err
	}
	return res, out, nil
}

func createLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.Create(path)
}

// lineWriter splits a byte stream into complete lines and hands each to a
// callback. Backends emit their progress a line at a time (one JSON object per
// event for a streaming CLI, plain prose otherwise), and a writer that fired
// per Write call would split mid-line on any buffer boundary.
//
// An over-long line with no newline in it — a base64 blob, a minified file
// echoed back — is flushed at maxLine rather than buffered without limit.
type lineWriter struct {
	fn  func(string)
	buf []byte
}

const maxLine = 1 << 16

func newLineWriter(fn func(string)) *lineWriter { return &lineWriter{fn: fn} }

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			if len(l.buf) > maxLine {
				l.emit(l.buf[:maxLine])
				l.buf = l.buf[maxLine:]
				continue
			}
			break
		}
		l.emit(l.buf[:i])
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

func (l *lineWriter) emit(b []byte) {
	if s := strings.TrimRight(string(b), "\r"); strings.TrimSpace(s) != "" {
		l.fn(s)
	}
}

// syncWriter serialises writes to a sink shared by two pump goroutines.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

type limitedWriter struct {
	w    io.Writer
	n    int64
	over bool // set once bytes have been dropped
}

// dropped is nil-safe so RunCapture can ask about a limiter it never built
// (maxBytes == 0, or stderr not separated) without a nil check at each site.
func (l *limitedWriter) dropped() bool { return l != nil && l.over }

func (l *limitedWriter) Write(p []byte) (int, error) {
	// full, not len(p) after the clamp below: os/exec copies through io.Copy,
	// which turns any short write into ErrShortWrite and fails the whole
	// command. A writer that exists to drop bytes therefore has to claim it
	// took them all, and `over` is the only record that it did not — which is
	// exactly why the Truncated flag has to be carried out of band.
	full := len(p)
	if l.n <= 0 {
		l.over = true
		return full, nil // swallow silently; caller keeps the head
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
		l.over = true
	}
	n, err := l.w.Write(p)
	l.n -= int64(n)
	return full, err
}

// adaptWindows wraps .bat/.cmd entrypoints in `cmd /c` and resolves
// repo-relative script names (gradlew.bat) against dir.
func adaptWindows(argv []string, dir string) []string {
	if runtime.GOOS != "windows" {
		return argv
	}
	head := argv[0]
	lower := strings.ToLower(head)
	if strings.HasSuffix(lower, ".bat") || strings.HasSuffix(lower, ".cmd") {
		if !filepath.IsAbs(head) && dir != "" {
			if _, err := os.Stat(filepath.Join(dir, head)); err == nil {
				head = filepath.Join(dir, head)
			}
		}
		return append([]string{"cmd", "/c", head}, argv[1:]...)
	}
	return argv
}

func overlayEnv(overlay map[string]string) []string {
	if len(overlay) == 0 {
		return nil // inherit
	}
	env := os.Environ()
	for k, v := range overlay {
		env = append(env, k+"="+v)
	}
	return env
}

// Excerpt extracts a failure excerpt from a log file: the last tailLines
// lines plus any earlier lines matching one of the compiled patterns
// (deduplicated, in order). SPEC §8.
func Excerpt(logPath string, tailLines int, patterns []*regexp.Regexp) string {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return fmt.Sprintf("(log unavailable: %v)", err)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	tailStart := len(lines) - tailLines
	if tailStart < 0 {
		tailStart = 0
	}
	var out []string
	for i, ln := range lines[:tailStart] {
		for _, p := range patterns {
			if p.MatchString(ln) {
				out = append(out, fmt.Sprintf("L%d: %s", i+1, ln))
				break
			}
		}
	}
	if len(out) > 0 {
		out = append(out, "...")
	}
	out = append(out, lines[tailStart:]...)
	return strings.Join(out, "\n")
}
