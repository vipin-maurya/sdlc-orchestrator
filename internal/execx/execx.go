// Package execx runs external commands with streaming log capture, timeouts,
// env overlays, and Windows .bat/.cmd handling (SPEC §8). No shell is ever
// involved except the cmd /c wrapper required for batch files.
package execx

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

type Cmd struct {
	Argv    []string
	Dir     string
	Env     map[string]string // overlaid on os.Environ()
	Timeout time.Duration     // 0 = no timeout
	LogPath string            // "" = discard; stdout+stderr interleaved
	Stdin   string            // literal stdin content ("" = none)
}

type Result struct {
	ExitCode int
	Duration time.Duration
	TimedOut bool
	LogPath  string
}

// Run executes the command. A non-zero exit is NOT a Go error; err is
// reserved for spawn/plumbing failures.
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
		if err := os.MkdirAll(filepath.Dir(c.LogPath), 0o755); err != nil {
			return Result{}, err
		}
		f, err := os.Create(c.LogPath)
		if err != nil {
			return Result{}, err
		}
		defer f.Close()
		sink = f
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
	if maxBytes > 0 {
		w = &limitedWriter{w: &sb, n: maxBytes}
	}
	cmd.Stdout = w
	cmd.Stderr = w
	start := time.Now()
	err := cmd.Run()
	res := Result{Duration: time.Since(start)}
	out := sb.String()
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res, out, nil
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, out, nil
		}
		return res, out, err
	}
	return res, out, nil
}

type limitedWriter struct {
	w io.Writer
	n int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil // swallow silently; caller keeps the head
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.w.Write(p)
	l.n -= int64(n)
	return len(p), err
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
