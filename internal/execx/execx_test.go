package execx

// The test binary impersonates the command under test (-echo-lines), so these
// run identically on every platform with no shell and no fixtures.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "-echo-lines" {
		for _, s := range os.Args[2:] {
			fmt.Println(s)
		}
		os.Exit(0)
	}
	// -noisy <stdout> <stderrBytes> <exitCode>: a command that talks on stderr
	// while producing a perfectly complete stdout, which is what a git diff
	// with a chatty textconv driver looks like.
	if len(os.Args) > 4 && os.Args[1] == "-noisy" {
		fmt.Print(os.Args[2])
		n, err := strconv.Atoi(os.Args[3])
		if err != nil {
			panic(err)
		}
		os.Stderr.Write(bytes.Repeat([]byte("e"), n))
		code, err := strconv.Atoi(os.Args[4])
		if err != nil {
			panic(err)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func echoArgv(t *testing.T, lines ...string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return append([]string{exe, "-echo-lines"}, lines...)
}

// The log has to exist and hold what the command has printed so far while the
// command is still running — that is the whole point of streaming it. Asserting
// it from inside the line callback proves it without sleeping on a race.
func TestRunCaptureWritesLogWhileRunning(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "nested", "run.log")
	var sawOnDisk []string
	var lines []string
	res, out, err := RunCapture(context.Background(), Cmd{
		Argv:    echoArgv(t, "first", "second", "third"),
		LogPath: logPath,
		Timeout: 30 * time.Second,
		OnLine: func(ln string) {
			lines = append(lines, ln)
			data, rerr := os.ReadFile(logPath)
			if rerr == nil && strings.Contains(string(data), ln) {
				sawOnDisk = append(sawOnDisk, ln)
			}
		},
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, out)
	}
	want := []string{"first", "second", "third"}
	if strings.Join(lines, ",") != strings.Join(want, ",") {
		t.Errorf("OnLine got %v, want %v", lines, want)
	}
	if strings.Join(sawOnDisk, ",") != strings.Join(want, ",") {
		t.Errorf("log lagged the callback: on disk %v, want %v", sawOnDisk, want)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range want {
		if !strings.Contains(string(data), w) {
			t.Errorf("final log missing %q:\n%s", w, data)
		}
	}
	if !strings.Contains(out, "second") {
		t.Errorf("captured output missing a line: %q", out)
	}
}

// A callback that fired per Write would split a line across two calls; the
// splitter must hand over whole lines regardless of chunk boundaries.
func TestLineWriterSplitsOnBoundaries(t *testing.T) {
	var got []string
	lw := newLineWriter(func(s string) { got = append(got, s) })
	for _, chunk := range []string{"al", "pha\nbe", "ta\n\n", "gamma"} {
		if _, err := lw.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, "|") != "alpha|beta" {
		t.Errorf("got %v, want [alpha beta] (gamma is unterminated)", got)
	}
	// CRLF is a line ending, not part of the line.
	got = nil
	lw = newLineWriter(func(s string) { got = append(got, s) })
	lw.Write([]byte("one\r\ntwo\r\n"))
	if strings.Join(got, "|") != "one|two" {
		t.Errorf("CRLF handling: got %v", got)
	}
}

// An unterminated line must not buffer without bound.
func TestLineWriterFlushesOverlongLine(t *testing.T) {
	var got []string
	lw := newLineWriter(func(s string) { got = append(got, s) })
	lw.Write([]byte(strings.Repeat("x", maxLine+10)))
	if len(got) != 1 || len(got[0]) != maxLine {
		t.Fatalf("want one %d-byte flush, got %d line(s)", maxLine, len(got))
	}
	if len(lw.buf) != 10 {
		t.Errorf("remainder buffered = %d bytes, want 10", len(lw.buf))
	}
}

// A capture that stopped at the cap and one that merely fit inside it return
// the same kind of string, so the flag is the only thing that tells them
// apart. Callers that stage a patch for a human to approve depend on it.
func TestRunCaptureReportsTruncation(t *testing.T) {
	line := strings.Repeat("x", 100)
	argv := echoArgv(t, line, line, line) // 303 bytes: 3 lines plus newlines

	// The cap falls inside a write rather than on its boundary, so this also
	// pins that hitting it is not an error: a limiter that reported the short
	// write honestly would make os/exec fail the command instead.
	const capBytes = 50
	res, out, err := RunCapture(context.Background(), Cmd{Argv: argv, Timeout: 30 * time.Second}, capBytes)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, out)
	}
	if !res.Truncated {
		t.Errorf("Truncated = false for %d bytes of output under a %d-byte cap", 303, capBytes)
	}
	if len(out) != capBytes {
		t.Errorf("captured %d bytes, want exactly %d", len(out), capBytes)
	}

	res, out, err = RunCapture(context.Background(), Cmd{Argv: argv, Timeout: 30 * time.Second}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated {
		t.Errorf("Truncated = true under a 1 MiB cap for %d bytes of output", len(out))
	}
	if len(out) != 303 {
		t.Errorf("captured %d bytes, want the whole 303", len(out))
	}

	// maxBytes == 0 builds no limiter at all; dropped() answers for the
	// limiter that does not exist rather than the caller checking for nil.
	res, out, err = RunCapture(context.Background(), Cmd{Argv: argv, Timeout: 30 * time.Second}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated {
		t.Errorf("Truncated = true with maxBytes 0 (unbounded)")
	}
	if len(out) != 303 {
		t.Errorf("captured %d bytes, want the whole 303", len(out))
	}
}

// noisyArgv builds a command that prints stdoutText, writes stderrBytes bytes
// of diagnostics, and exits with code.
func noisyArgv(t *testing.T, stdoutText string, stderrBytes, code int) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe, "-noisy", stdoutText, strconv.Itoa(stderrBytes), strconv.Itoa(code)}
}

// stderrCap mirrors the fixed 1<<16 cap RunCapture puts on the separated
// stderr buffer. It is not derived from maxBytes, so it bites even when the
// caller asked for an unbounded capture.
const stderrCap = 1 << 16

// The pair below is the whole invariant: Truncated describes the string the
// call returned, so the same stderr overflow means different things on the two
// return paths.
//
// Success returns stdout alone. A command can flood stderr — a git diff with a
// chatty textconv driver does exactly this, at exit 0 — while its stdout is
// complete to the last byte. Marking that capture truncated tells a reviewer a
// whole patch is "its head", and a flag that cries wolf gets ignored when it
// finally matters.
func TestRunCaptureStderrOverflowLeavesSuccessUntruncated(t *testing.T) {
	payload := strings.Repeat("p", 300)
	res, out, err := RunCapture(context.Background(), Cmd{
		Argv:           noisyArgv(t, payload, stderrCap+4096, 0),
		StderrSeparate: true,
		Timeout:        30 * time.Second,
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d, want 0", res.ExitCode)
	}
	if out != payload {
		t.Fatalf("stdout capture is %d bytes, want the whole %d", len(out), len(payload))
	}
	if res.Truncated {
		t.Errorf("Truncated = true for a complete %d-byte stdout capture; stderr overflowed but stderr is not in the returned string", len(out))
	}
}

// The failure path returns stdout with the stderr capture appended, so bytes
// the stderr limiter dropped are bytes missing from what the caller holds —
// and there the flag must fire.
func TestRunCaptureStderrOverflowTruncatesFailure(t *testing.T) {
	payload := strings.Repeat("p", 300)
	res, out, err := RunCapture(context.Background(), Cmd{
		Argv:           noisyArgv(t, payload, stderrCap+4096, 3),
		StderrSeparate: true,
		Timeout:        30 * time.Second,
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit %d, want 3", res.ExitCode)
	}
	if len(out) != len(payload)+stderrCap {
		t.Fatalf("returned %d bytes, want stdout (%d) plus a capped stderr (%d)", len(out), len(payload), stderrCap)
	}
	if !res.Truncated {
		t.Errorf("Truncated = false although the returned string carries a stderr capture clipped at %d bytes", stderrCap)
	}

	// Same path, stderr that fits: nothing was dropped from the returned
	// string, so the flag stays down. Without this the fix could degenerate
	// into "every failure is truncated".
	res, out, err = RunCapture(context.Background(), Cmd{
		Argv:           noisyArgv(t, payload, 10, 3),
		StderrSeparate: true,
		Timeout:        30 * time.Second,
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(payload)+10 {
		t.Fatalf("returned %d bytes, want %d", len(out), len(payload)+10)
	}
	if res.Truncated {
		t.Errorf("Truncated = true although both captures are complete")
	}
}

// The cap is a limit on what fits, not on what is allowed to arrive: output of
// exactly maxBytes bytes is complete. The doc comments turn on this boundary,
// so pin all three sides of it — an off-by-one here would silently relabel
// every exactly-at-cap capture.
func TestRunCaptureCapBoundary(t *testing.T) {
	line := strings.Repeat("x", 199)
	argv := echoArgv(t, line) // 200 bytes: the line plus its newline
	const total = 200
	for _, tc := range []struct {
		cap  int64
		want bool
	}{
		{total - 1, true},
		{total, false},
		{total + 1, false},
	} {
		res, out, err := RunCapture(context.Background(), Cmd{Argv: argv, Timeout: 30 * time.Second}, tc.cap)
		if err != nil {
			t.Fatal(err)
		}
		if res.Truncated != tc.want {
			t.Errorf("cap %d over %d bytes: Truncated = %v, want %v (captured %d)", tc.cap, total, res.Truncated, tc.want, len(out))
		}
	}
}

// limitedWriter's own boundary, away from os/exec's chunking, plus the
// zero-length write: an empty write offers no bytes, so it can drop none. Only
// io.Copy's nr>0 guard keeps that one out of reach today.
func TestLimitedWriterBoundaryAndEmptyWrite(t *testing.T) {
	var sb strings.Builder
	l := &limitedWriter{w: &sb, n: 4}
	if n, err := l.Write([]byte("abcd")); n != 4 || err != nil {
		t.Fatalf("Write = (%d, %v), want (4, nil)", n, err)
	}
	if l.dropped() {
		t.Errorf("dropped = true after a write of exactly the budget")
	}
	if n, err := l.Write(nil); n != 0 || err != nil {
		t.Fatalf("empty Write = (%d, %v), want (0, nil)", n, err)
	}
	if l.dropped() {
		t.Errorf("dropped = true after an empty write on an exhausted budget: no bytes were offered, so none went missing")
	}
	if _, err := l.Write([]byte("e")); err != nil {
		t.Fatal(err)
	}
	if !l.dropped() {
		t.Errorf("dropped = false after a real write past the budget")
	}
	if sb.String() != "abcd" {
		t.Errorf("wrote %q, want %q", sb.String(), "abcd")
	}

	// A zero-budget writer is at its cap from the start; the same rule holds.
	zero := &limitedWriter{w: &sb, n: 0}
	if n, err := zero.Write([]byte{}); n != 0 || err != nil {
		t.Fatalf("empty Write on a zero-budget writer = (%d, %v), want (0, nil)", n, err)
	}
	if zero.dropped() {
		t.Errorf("zero-budget writer reports dropped bytes after being offered none")
	}
	if _, err := zero.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if !zero.dropped() {
		t.Errorf("zero-budget writer swallowed a byte without reporting it")
	}
}
