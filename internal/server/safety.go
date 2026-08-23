package server

// The checks that exist to stop something bad, in one file with the reasoning
// attached — the same arrangement, and for the same reason, as internal/guard.
// This server has no authentication: its access control is that the listener is
// on the loopback interface and the operator is the only person on the machine.
// Every check below exists because some other party can reach a loopback
// listener anyway, and each comment says which one.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// csrfCookie is the browser-side half of the double-submit pair.
const csrfCookie = "sdlc_csrf"

// errUnsafeName and errNoSuchName separate "this name may not be asked for"
// from "this name is not here", because the caller answers 400 for the first
// and 404 for the second and the two must not be conflated: a 404 for a
// traversal attempt would tell the caller that a different name might work.
var (
	errUnsafeName = errors.New("unsafe name")
	errNoSuchName = errors.New("no such file in directory")
)

// safeName resolves name to a file inside dir by MEMBERSHIP in os.ReadDir(dir),
// never by cleaning and prefix-comparing.
//
// Prefix comparison after filepath.Clean is defeatable on Windows by
// drive-relative names (C:file resolves against the process's current directory
// on drive C:, not against dir), alternate data streams (name:$DATA opens a
// different stream of the same file) and 8.3 short names (PROGRA~1 names a
// directory whose long name the prefix check never saw). A name that is not in
// the directory listing is not in the directory, on every platform, and that is
// the only property this function relies on.
//
// A symlink is in the listing but its target need not be in the directory, so
// only regular files resolve. Following the link and re-checking the target
// would be a race the attacker wins by swapping the link after the check; the
// class is refused instead.
func safeName(dir, name string) (string, error) {
	// Rejected before the listing is read so a hostile name cannot even cause a
	// directory to be opened, and so the error is the 400 the caller wants.
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%q: %w", name, errUnsafeName)
	}
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("%q: %w", name, errUnsafeName)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("%q: %w", name, errNoSuchName)
	}
	for _, e := range ents {
		if e.Name() != name {
			continue
		}
		if !e.Type().IsRegular() {
			return "", fmt.Errorf("%q: %w", name, errUnsafeName)
		}
		return filepath.Join(dir, name), nil
	}
	return "", fmt.Errorf("%q: %w", name, errNoSuchName)
}

// hostAllowed reports whether the Host header names this server.
//
// Without it, DNS rebinding turns "the listener is on loopback" into no
// protection at all: an attacker publishes evil.example with a one-second TTL,
// the operator's browser loads a page from it, the name is re-resolved to
// 127.0.0.1, and the attacker's JavaScript is then same-origin with this server
// and can click approve. The browser sends the attacker's name in Host, which
// is the one part of the request the attack cannot forge, so refusing every
// Host that is not a spelling of this machine refuses the whole attack.
//
// Only the three loopback spellings are accepted. A name that merely contains
// one (127.0.0.1.evil.example) is a different host, and an absent Host is not a
// request any browser makes.
func hostAllowed(hostport string) bool {
	if hostport == "" {
		return false
	}
	h := hostport
	if x, port, err := net.SplitHostPort(hostport); err == nil {
		// The port has to be digits. SplitHostPort is happy to call
		// "7777.evil.example" a port and hand back 127.0.0.1 as the host, which
		// would let a Host this function is meant to refuse through on the
		// strength of the part before the colon. No browser can be made to send
		// it — a non-numeric port makes the URL invalid, and Host is a
		// forbidden header for fetch — but a check whose comment says three
		// spellings should accept three spellings.
		if port != "" && strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return false
		}
		h = x
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		// SplitHostPort rejects a bracketed literal with no port; unwrap it here
		// so "[::1]" is recognised as the same host as "[::1]:7777".
		h = h[1 : len(h)-1]
	}
	switch h {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// checkHost answers 421 Misdirected Request to anything that is not addressed
// to this machine. 421 rather than 400 because it is exactly true: the request
// arrived at a server that is not authoritative for the name it names.
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host) {
			// Plain text, not the error page: the response must carry no
			// application content at all, so a rebinding attempt learns
			// nothing about what is running here.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusMisdirectedRequest)
			fmt.Fprintln(w, "421 misdirected request: this server answers only to localhost")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// newCSRFToken is 32 bytes of crypto/rand. math/rand would be seeded from a
// value an attacker can often guess within a few seconds of process start.
func newCSRFToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// withCSRFCookie ensures every request carries a token, and stashes it where
// page() can read it so the same value goes into the form.
//
// The pair is double-submit rather than a server-side session because there is
// nothing to hold a session against: no users, no login, no actor column in
// approvals (spec §2.2). The property that makes it work is that a cross-origin
// page can cause the cookie to be sent but cannot read it, so it cannot put a
// matching value in the form body.
func (s *Server) withCSRFCookie(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := ""
		if c, err := r.Cookie(csrfCookie); err == nil {
			tok = c.Value
		}
		if tok == "" {
			var err error
			if tok, err = newCSRFToken(); err != nil {
				http.Error(w, "500 cannot generate a CSRF token", http.StatusInternalServerError)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:  csrfCookie,
				Value: tok,
				Path:  "/",
				// HttpOnly: the token is only ever compared server-side, so no
				// script needs to read it, and a script that cannot read it
				// cannot leak it through an XSS that slipped past the escaping.
				HttpOnly: true,
				// Lax, not Strict: Strict would drop the cookie on a link
				// followed from another application, leaving the operator on a
				// page whose forms all fail. Lax still withholds it from a
				// cross-site POST, which is the request this defends against.
				SameSite: http.SameSiteLaxMode,
			})
		}
		// Carried on the context rather than a header so a client cannot
		// supply its own, and so page() reads the same string on the request
		// that just minted the cookie — a Set-Cookie is not readable back out
		// of r.Cookie.
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfKey{}, tok)))
	})
}

// csrfKey types the context slot holding the request's token.
type csrfKey struct{}

// csrfToken returns the token withCSRFCookie put on the request, or "" if the
// middleware did not run (which no served route can arrange).
func csrfToken(r *http.Request) string {
	t, _ := r.Context().Value(csrfKey{}).(string)
	return t
}

// requireCSRF rejects a POST whose form token does not match the cookie.
func (s *Server) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			// A body over the MaxBytesReader limit lands here, which is the
			// 413 the bounds table asks for rather than a confusing 403.
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				s.fail(w, r, http.StatusRequestEntityTooLarge, "the form is larger than 1 MiB")
				return
			}
			s.fail(w, r, http.StatusBadRequest, "the form could not be read")
			return
		}
		cookie := ""
		if c, err := r.Cookie(csrfCookie); err == nil {
			cookie = c.Value
		}
		form := r.PostFormValue("csrf")
		// Both halves must exist: a comparison of "" against "" succeeds, so a
		// request that sends neither would otherwise pass the check.
		if cookie == "" || form == "" ||
			subtle.ConstantTimeCompare([]byte(cookie), []byte(form)) != 1 {
			s.fail(w, r, http.StatusForbidden,
				"this form did not come from a page this server served — reload the page and try again")
			return
		}
		next(w, r)
	}
}

// freshHandler runs only after the job has been re-read and its state matched
// against the form's `state` field.
type freshHandler func(w http.ResponseWriter, r *http.Request, j *store.Job)

// requireFreshState refuses a decision posted from a page that no longer
// describes the job.
//
// A browser tab left open on a gate page is a live approve button pointed at a
// job that may have moved on hours ago — the merge it approves is not the merge
// the operator read. The form carries the state the page was rendered from; if
// the job has since moved, nothing is written and the answer is 409 naming what
// the job is doing now (spec §10.3).
func (s *Server) requireFreshState(next freshHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		j, ok := s.job(w, r)
		if !ok {
			return
		}
		want := r.PostFormValue("state")
		if want == "" {
			s.fail(w, r, http.StatusBadRequest,
				"the form carried no job state, so there is nothing to check it against")
			return
		}
		if j.State != want {
			s.fail(w, r, http.StatusConflict, fmt.Sprintf(
				"%s was %s when this page was rendered and is %s now, so the decision was not recorded — reload %s and read it again",
				j.ID, want, j.State, "/jobs/"+j.ID))
			return
		}
		next(w, r, j)
	}
}

// recoverPanic keeps one bad request from taking the server down with it. It is
// outermost so it also covers the checks below it; a panic inside checkHost
// that killed the process would be a denial of service reachable from any
// origin, since the Host check is what would otherwise have rejected it.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the documented way a handler says "stop,
			// quietly" — an SSE client that went away raises it, and logging a
			// stack for that would bury the panics that matter.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			s.log.Printf("panic serving %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
			// The message names nothing about the failure: the stack is in the
			// operator's console, which is a better place for it than a page
			// that may be open in a tab for hours.
			defer func() { _ = recover() }() // the response may already be past its header
			http.Error(w, "500 internal error — see the sdlc serve console", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}
