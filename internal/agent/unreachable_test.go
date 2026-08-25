package agent

import (
	"strings"
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
)

// jobTwoEnvelope is the exact line the claude CLI wrote when a DNS failure
// killed JOB-2 on 2026-08-24 (data/jobs/JOB-2/logs/047_PLANNING_opus.log,
// trimmed to the fields that matter). The engine tests drive the routing with
// a synthetic marker; this file is the one that pins the real bytes, because
// the classification is only worth anything if it fires on what the CLI
// actually emits.
const jobTwoEnvelope = `{"is_error":true,"duration_api_ms":0,"num_turns":1,` +
	`"stop_reason":"stop_sequence","session_id":"ee817b33","total_cost_usd":0,` +
	`"terminal_reason":"api_error","fast_mode_state":"off","api_error_status":null,` +
	`"result":"API Error: Can't reach the API server — check your internet or DNS (ENOTFOUND)",` +
	`"type":"result","duration_ms":178937}`

// The structured field is authoritative and must classify on its own, with no
// help from the pattern list — CLI prose changes, terminal_reason does not.
func TestTerminalReasonAPIErrorIsUnreachable(t *testing.T) {
	// Deliberately passes no patterns: terminal_reason alone must be enough,
	// or the classification is really just the pattern list wearing a hat.
	res := Result{ExitCode: 1, Stdout: jobTwoEnvelope}
	parseEnvelope(jobTwoEnvelope, &res)
	classifyTransport(&res, nil)
	if !res.Unreachable {
		t.Error("terminal_reason api_error did not classify as unreachable")
	}
	// The same envelope carries is_error, which must still force a failing exit
	// code — classification must not swallow the failure itself.
	if res.ExitCode == 0 {
		t.Error("is_error envelope left a zero exit code")
	}
}

// A normal failed run — the model answered, the answer was wrong — must NOT be
// classified as unreachable, or a genuinely broken agent would retry forever
// instead of escalating.
func TestOrdinaryFailureIsNotUnreachable(t *testing.T) {
	for _, out := range []string{
		`{"is_error":true,"terminal_reason":"max_turns","result":"I could not find the file"}`,
		`{"is_error":false,"result":"done"}`,
		`{"is_error":true,"terminal_reason":"refusal","result":"I will not do that"}`,
	} {
		res := Result{ExitCode: 1, Stdout: out}
		parseEnvelope(out, &res)
		classifyTransport(&res, config.DefaultTransportPatternsForTest())
		if res.Unreachable {
			t.Errorf("ordinary failure classified as unreachable: %s", out)
		}
	}
}

// The shipped patterns are the fallback for a CLI that dies without an
// envelope. Every string here is one a Node CLI emits verbatim.
func TestDefaultTransportPatternsMatchRealErrors(t *testing.T) {
	pats := config.DefaultTransportPatternsForTest()
	for _, out := range []string{
		"Error: getaddrinfo ENOTFOUND api.anthropic.com",
		"FetchError: request to https://api.anthropic.com failed, reason: connect ECONNREFUSED 127.0.0.1:443",
		"Error: socket hang up",
		"Error: connect ETIMEDOUT",
		"API Error: Can't reach the API server",
		"Error: connect EHOSTUNREACH 10.0.0.1:443",
		"getaddrinfo EAI_AGAIN api.anthropic.com",
	} {
		if !matchesAny(out, pats) {
			t.Errorf("transport error not matched: %q", out)
		}
	}
}

// An agent working on THIS repository writes about ECONNREFUSED and socket
// timeouts in the ordinary course of its job. Suspending a job because the
// agent used the word would be a worse bug than the one this classification
// fixes, so the decision is scoped to the CLI's own message — never the
// transcript. This is the test that pins that, and it is why the patterns
// alone are not the protection.
func TestOrdinaryAgentOutputIsNotUnreachable(t *testing.T) {
	pats := config.DefaultTransportPatternsForTest()
	cases := []struct {
		name string
		out  string
		exit int
	}{
		{"succeeded while discussing errnos", `{"is_error":false,"result":"Added a test for the ECONNREFUSED branch"}`, 0},
		{"failed for its own reasons, transcript mentions errnos",
			`{"is_error":true,"terminal_reason":"max_turns","result":"I was editing the ETIMEDOUT handling and ran out of turns"}`, 1},
		{"no envelope, prose about networking, exit 0",
			"I read internal/net/dial.go; the getaddrinfo path looks correct", 0},
		{"spec text about limits", `{"is_error":false,"result":"The spec mentions rate limits and quota handling"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Result{ExitCode: tc.exit, Stdout: tc.out}
			parseEnvelope(tc.out, &res)
			classifyTransport(&res, pats)
			if res.Unreachable {
				t.Errorf("ordinary output classified as unreachable: %q", tc.out)
			}
		})
	}
}

// The counterpart: a CLI that really could not reach the API must classify,
// through the envelope path and the no-envelope path alike.
func TestRealTransportFailuresClassify(t *testing.T) {
	pats := config.DefaultTransportPatternsForTest()
	cases := []struct{ name, out string }{
		{"claude envelope with terminal_reason", jobTwoEnvelope},
		{"envelope without terminal_reason",
			`{"is_error":true,"result":"request failed, reason: connect ECONNREFUSED 127.0.0.1:443"}`},
		{"no envelope at all", "Error: getaddrinfo ENOTFOUND api.anthropic.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Result{ExitCode: 1, Stdout: tc.out}
			parseEnvelope(tc.out, &res)
			classifyTransport(&res, pats)
			if !res.Unreachable {
				t.Errorf("transport failure not classified: %q", tc.out)
			}
		})
	}
}

// The hold reason is the only place an operator sees why the job stopped, so
// it has to be the backend's sentence and not a generic stand-in.
func TestUnreachableDetailPrefersTheCLIsOwnMessage(t *testing.T) {
	got := UnreachableDetail(jobTwoEnvelope)
	if !strings.Contains(got, "Can't reach the API server") {
		t.Errorf("detail = %q, want the CLI's own result field", got)
	}
}

// No envelope is the common case for a CLI killed mid-request.
func TestUnreachableDetailFallsBackToAMatchingLine(t *testing.T) {
	got := UnreachableDetail("starting up\nError: getaddrinfo ENOTFOUND api.anthropic.com\n")
	if !strings.Contains(got, "ENOTFOUND") {
		t.Errorf("detail = %q, want the errno line", got)
	}
}

func TestUnreachableDetailAlwaysSaysSomething(t *testing.T) {
	if got := UnreachableDetail(""); got == "" {
		t.Error("empty output produced an empty hold reason")
	}
}

// A multi-line message must arrive as one bounded line: it goes into a console
// notice and a job page, both of which have a width.
func TestUnreachableDetailIsOneBoundedLine(t *testing.T) {
	got := UnreachableDetail("{\"result\":\"API Error: Can't reach the API server\\nstack line 1\\nstack line 2\"}")
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("detail spans lines: %q", got)
	}
}
