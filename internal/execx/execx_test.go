package execx

// The test binary impersonates the command under test (-echo-lines), so these
// run identically on every platform with no shell and no fixtures.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
