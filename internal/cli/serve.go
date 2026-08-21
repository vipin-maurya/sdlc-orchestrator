package cli

// `sdlc serve` hosts the local web UI. It is the one command in this package
// that opens a socket, so the whole of what keeps that socket private lives
// here and in config.NormalizeListen: the server has no authentication, no
// users and no roles, and its trust boundary is "this machine". An address
// that reaches beyond loopback does not weaken that boundary, it removes it —
// what is published is an approve button.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/server"
)

// shutdownGrace bounds how long a stopping server waits for the requests
// already in flight. It is not zero because the request most likely to be in
// flight is an approval POST: cutting it off mid-write leaves the operator
// having clicked approve with no row to show for it, and no way to tell
// whether the click landed. It is not unbounded because a ctrl-c that does not
// visibly stop the process invites a second, harder one.
const shutdownGrace = 5 * time.Second

func cmdServe(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "", "override server.listen (loopback only; port 0 picks a free port)")
	verbose := fs.Bool("v", false, "log every request")
	if _, err := parseArgs(fs, args, 0, "sdlc serve [--addr 127.0.0.1:7777] [--v]"); err != nil {
		return argFail(err)
	}
	listen, code, err := resolveListen(cfg, *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return code
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServe(ctx, cfg, listen, *verbose, os.Stdout)
}

// resolveListen returns the exact string to hand to net.Listen, along with the
// exit code for a refusal: a bad --addr is a usage error (2) and a bad
// server.listen is a configuration error (1), which is the code every other
// command already uses for a config it cannot act on.
//
// Both paths go through config.NormalizeListen and the caller binds its return
// value. Validating the operator's spelling and then binding that same
// spelling is the bug this indirection exists to prevent: "localhost" is a
// name, and a name is resolved at bind time by whatever /etc/hosts or DNS says
// then — possibly to a routable address that no check ever saw.
//
// allowPortZero is true only for --addr. An ephemeral port is not something an
// operator can usefully write in a config file, but it is how a test binds
// without racing another test for a fixed one.
func resolveListen(cfg *config.Config, addr string) (string, int, error) {
	if addr != "" {
		listen, err := config.NormalizeListen(addr, true)
		if err != nil {
			return "", 2, fmt.Errorf("--addr %v", err)
		}
		return listen, 0, nil
	}
	listen, err := config.NormalizeListen(cfg.Server.Listen, false)
	if err != nil {
		return "", 1, fmt.Errorf("server.listen %v", err)
	}
	return listen, 0, nil
}

// runServe binds listen, serves until ctx is cancelled, and shuts down. It
// takes its context and its announcement writer as arguments so a test can
// drive the whole path — bind, answer, stop — without a signal and without
// racing for a port; the signal wiring that a person actually uses is one
// level up, in cmdServe.
func runServe(ctx context.Context, cfg *config.Config, listen string, verbose bool, out io.Writer) int {
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()

	// The server's log shares this console with the address banner and with any
	// panic the recover middleware reports, so it goes to the same stream
	// rather than to stderr, where the two would interleave unpredictably.
	srv, err := server.New(cfg, st, log.New(out, "", log.LstdFlags), verbose)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		// Naming the address matters more than the syscall text: "address
		// already in use" without it leaves the operator guessing whether they
		// have another sdlc serve running or something else owns the port.
		fmt.Fprintf(os.Stderr, "error: cannot listen on %s: %v\n", listen, err)
		return 1
	}

	// ln.Addr(), not listen: with port 0 the useful port is the one the kernel
	// picked, and when the operator wrote "localhost" the address that was
	// actually bound is 127.0.0.1. Printing what they typed instead of what
	// exists is how somebody ends up debugging an address nobody bound.
	fmt.Fprintf(out, "sdlc serve listening on http://%s/\n", ln.Addr())
	fmt.Fprintln(out, "  loopback only, and it has no authentication: anyone who can reach this address can approve a merge.")
	fmt.Fprintln(out, "  to reach it from another machine, forward the port: ssh -L 7777:127.0.0.1:7777 host")
	fmt.Fprintln(out, "  press ctrl-c to stop")

	// Buffered: after Shutdown below, Serve returns ErrServerClosed into this
	// channel with nobody left to read it, and an unbuffered send would leak
	// the goroutine for the rest of the process's life.
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		// Serve stopped on its own, which at this point means the listener
		// broke — a Shutdown has not been asked for yet.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	case <-ctx.Done():
	}

	fmt.Fprintln(out, "shutting down; waiting up to", shutdownGrace, "for requests in flight")
	// context.Background, not ctx: ctx is already cancelled — that is why we
	// are here — so passing it would give in-flight requests no grace at all.
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		fmt.Fprintln(os.Stderr, "error: shutdown:", err)
		return 1
	}
	return 0
}
