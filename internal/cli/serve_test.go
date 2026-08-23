package cli

// Tests for `sdlc serve`. The address it binds is the whole of this server's
// access control — there is no login behind it — so the two things asserted
// here are that a routable address is refused with advice the operator can act
// on, and that what actually ends up bound is the normalized loopback literal
// rather than whatever string was typed.

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
)

// A non-loopback --addr must be refused, and the refusal must say how to reach
// the server from another machine anyway. A refusal that only says no is how
// the address ends up at 0.0.0.0 on the second attempt.
func TestServeRejectsNonLoopbackAddr(t *testing.T) {
	e := newReviewEnv(t)
	for _, addr := range []string{"0.0.0.0:7777", "192.168.1.10:7777", ":7777", "[::]:7777"} {
		t.Run(addr, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() { code = cmdServe(e.cfg, []string{"--addr", addr}) })
			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if !strings.Contains(stderr, "--addr") {
				t.Errorf("refusal does not name the flag: %q", stderr)
			}
			if !strings.Contains(stderr, "ssh -L") || !strings.Contains(stderr, "no authentication") {
				t.Errorf("refusal carries no advice: %q", stderr)
			}
			if strings.Contains(stderr, "listening") {
				t.Errorf("refused address still announced: %q", stderr)
			}
		})
	}
}

// A hostname is refused rather than resolved, and "localhost" is rewritten to
// the literal that gets bound: resolveListen's answer, not the operator's
// spelling, is what runServe hands to net.Listen.
func TestServeResolvesToALoopbackLiteral(t *testing.T) {
	e := newReviewEnv(t)
	cases := []struct {
		addr, want string
	}{
		{"localhost:7777", "127.0.0.1:7777"},
		{"localhost:0", "127.0.0.1:0"},
		{"127.0.0.1:7777", "127.0.0.1:7777"},
		{"[::1]:7777", "[::1]:7777"},
	}
	for _, tc := range cases {
		got, code, err := resolveListen(e.cfg, tc.addr)
		if err != nil {
			t.Errorf("resolveListen(%q): %v", tc.addr, err)
			continue
		}
		if code != 0 {
			t.Errorf("resolveListen(%q) code = %d, want 0", tc.addr, code)
		}
		if got != tc.want {
			t.Errorf("resolveListen(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
	// Port 0 is usable from the flag and refused from the file: an ephemeral
	// port is how a test binds without racing, and is not a thing anybody can
	// usefully write in a config.
	e.cfg.Server.Listen = "127.0.0.1:0"
	if _, code, err := resolveListen(e.cfg, ""); err == nil {
		t.Error("server.listen with port 0 accepted")
	} else if code != 1 {
		t.Errorf("config refusal exit = %d, want 1", code)
	}
}

// The end-to-end path: bind, announce, answer, stop. Port 0 so nothing races
// for 7777, and the announced address is then dialled — which is what proves
// the printed line names the socket that exists rather than the string that
// was configured.
func TestServeBindsAndAnswersHealthz(t *testing.T) {
	e := newReviewEnv(t)
	// "localhost" on purpose: what must be bound is the 127.0.0.1 literal
	// NormalizeListen returns, not the name, which /etc/hosts could point at a
	// routable interface.
	listen, _, err := resolveListen(e.cfg, "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServe(ctx, e.cfg, listen, false, out) }()

	line := waitForLine(t, out, "listening on")
	addr := strings.TrimSuffix(strings.TrimPrefix(strings.Fields(line)[len(strings.Fields(line))-1], "http://"), "/")
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("announced address %q is not host:port: %v", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("bound host %q is not a loopback literal (announced %q)", host, addr)
	}
	if port == "0" {
		t.Fatalf("announced the port that was asked for, not the one that was bound: %q", addr)
	}

	res, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz on the announced address: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", res.StatusCode)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("runServe exit = %d, want 0", code)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runServe did not return after its context was cancelled")
	}
	if !strings.Contains(out.String(), "shutting down") {
		t.Errorf("shutdown was not announced: %q", out.String())
	}
	// A cancelled context must not leave the port held: nothing else can bind
	// it until the process exits, which is what makes ctrl-c-then-restart fail
	// with "address already in use".
	ln, err := net.Listen("tcp", host+":"+port)
	if err != nil {
		t.Fatalf("port still held after shutdown: %v", err)
	}
	ln.Close()
}

// A port already in use must fail with the address named. "address already in
// use" on its own does not tell the operator which address.
func TestServeReportsATakenPort(t *testing.T) {
	e := newReviewEnv(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	var code int
	stderr := captureStderr(t, func() {
		code = runServe(context.Background(), e.cfg, held.Addr().String(), false, &syncBuf{})
	})
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, held.Addr().String()) {
		t.Errorf("failure does not name the address: %q", stderr)
	}
}

// The default config must be servable as it ships: an operator who writes no
// server block still gets a loopback address.
func TestDefaultConfigListensOnLoopback(t *testing.T) {
	listen, code, err := resolveListen(config.Default(), "")
	if err != nil {
		t.Fatalf("default server.listen is not bindable: %v (exit %d)", err, code)
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Errorf("default listen host %q is not loopback", host)
	}
}

// syncBuf is a bytes.Buffer the test can read while runServe's goroutines are
// still writing to it. Without the mutex the read below is a data race, and
// the whole point of the test is to read the address while the server runs.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitForLine polls until a line containing want appears, rather than sleeping
// a guessed interval: binding is fast but not instantaneous, and a fixed sleep
// is either flaky or slow.
func waitForLine(t *testing.T, out *syncBuf, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, ln := range strings.Split(out.String(), "\n") {
			if strings.Contains(ln, want) {
				return ln
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no line containing %q; got:\n%s", want, out.String())
	return ""
}
