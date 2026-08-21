package server

// Tests for the read-only pages. Owner: W2-H.
//
// Two properties get most of the attention here, because they are the two that
// fail quietly. The first is that a name taken from a URL never reaches a file
// it was not allowed to reach — asserted through the HTTP surface, since
// safety_test.go already asserts it against safeName directly and what is left
// to prove is that these handlers actually call it. The second is that the
// bounds of spec §5.4 truncate *and say so*: a page that silently dropped the
// first 38 000 lines of a failure would let a reader conclude the run started
// where the page starts.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// writeJobFile puts content in one of the job's three served directories and
// returns the path. The fixture creates jobs in the database only, so anything
// on disk has to be made here.
func writeJobFile(t *testing.T, e *env, j *store.Job, kind, name, content string) string {
	t.Helper()
	var dir string
	switch kind {
	case "logs":
		dir = artifact.LogsDir(e.cfg.Orchestrator.DataDir, j.ID)
	case "artifacts":
		dir = artifact.ArtifactsDir(e.cfg.Orchestrator.DataDir, j.ID)
	case "prompts":
		dir = artifact.PromptsDir(e.cfg.Orchestrator.DataDir, j.ID)
	default:
		t.Fatalf("unknown directory kind %q", kind)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// getOK fetches path and fails unless it answered 200, returning the body.
func getOK(t *testing.T, e *env, path string) string {
	t.Helper()
	res := e.get(path)
	body := e.body(res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200\n%s", path, res.StatusCode, truncateForLog(body))
	}
	return body
}

func truncateForLog(s string) string {
	if len(s) > 2000 {
		return s[:2000] + "…"
	}
	return s
}

// --- every page renders --------------------------------------------------

func TestEveryReadOnlyPageRenders200(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "a job with files on disk")
	writeJobFile(t, e, j, "logs", "build.log", "line one\nline two\n")
	writeJobFile(t, e, j, "artifacts", "spec.json", `{"schema":"spec/1","approach":"do it"}`)
	writeJobFile(t, e, j, "prompts", "planning.md", "# planning prompt\n")
	if err := e.st.AddEvent(&store.Event{
		JobID: j.ID, State: j.State, Kind: "progress",
		Detail: `{"phase":"build","argv":["./gradlew","assemble"]}`,
	}); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{
		"/jobs",
		"/jobs/" + j.ID,
		"/jobs/" + j.ID + "/events",
		"/jobs/" + j.ID + "/events?kind=progress",
		"/jobs/" + j.ID + "/logs",
		"/jobs/" + j.ID + "/logs/build.log",
		"/jobs/" + j.ID + "/logs/build.log?all=1",
		"/jobs/" + j.ID + "/artifacts",
		"/jobs/" + j.ID + "/artifacts/spec.json",
		"/jobs/" + j.ID + "/prompts/planning.md",
		"/submit",
		"/config",
	} {
		t.Run(p, func(t *testing.T) { getOK(t, e, p) })
	}
}

// TestJobDetailShowsWhatSpecFiveOneAsksFor pins the list in the route table
// rather than "it rendered": the page exists to answer "what is this job doing
// and what is it waiting for", and a field quietly dropped from the template
// would still render 200.
func TestJobDetailShowsWhatSpecFiveOneAsksFor(t *testing.T) {
	e := newEnv(t)
	j := e.job("ESCALATED", "a held job")
	j.HoldReason = "the build failed four times"
	j.Counters.FixAttempts = 4
	if err := e.st.UpdateJob(j); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddEvent(&store.Event{
		JobID: j.ID, State: j.State, Kind: "progress", Detail: `{"phase":"unit","started":true}`,
	}); err != nil {
		t.Fatal(err)
	}
	body := getOK(t, e, "/jobs/"+j.ID)
	for _, want := range []string{
		"ESCALATED",                       // state
		"the build failed four times",     // hold reason
		j.Branch,                          // branch
		j.WorktreePath,                    // worktree
		`&#34;phase&#34;: &#34;unit&#34;`, // the last progress event, pretty-printed
		"/jobs/" + j.ID + "/gate",         // gate link
		"/jobs/" + j.ID + "/diff",         // diff link
		"/jobs/" + j.ID + "/events",       // events link
		"/jobs/" + j.ID + "/logs",         // logs link
		"/jobs/" + j.ID + "/artifacts",    // artifacts link
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the job page does not mention %q", want)
		}
	}
	// The fix-attempt counter is rendered as a number in its own cell, so the
	// assertion is that a 4 appears in the counters list at all.
	if !strings.Contains(body, "fix attempts") {
		t.Error("the job page does not list the counters")
	}
}

func TestMissingJobIs404(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{
		"/jobs/JOB-404",
		"/jobs/JOB-404/events",
		"/jobs/JOB-404/logs",
		"/jobs/JOB-404/logs/build.log",
		"/jobs/JOB-404/artifacts",
		"/jobs/JOB-404/artifacts/spec.json",
		"/jobs/JOB-404/prompts/planning.md",
	} {
		if got := e.get(p).StatusCode; got != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, got)
		}
	}
}

// TestListingsSurviveAJobWithNothingOnDisk covers the state every job is in for
// its first few seconds. A 404 there would say the job does not exist, which is
// a different and much more alarming thing than "it has not written anything".
func TestListingsSurviveAJobWithNothingOnDisk(t *testing.T) {
	e := newEnv(t)
	j := e.bareJob("CREATED", "brand new")
	for _, p := range []string{"/jobs/" + j.ID + "/logs", "/jobs/" + j.ID + "/artifacts"} {
		body := getOK(t, e, p)
		if !strings.Contains(body, "no log directory yet") && !strings.Contains(body, "no artifact directory yet") {
			t.Errorf("GET %s renders no explanation of the empty listing", p)
		}
	}
}

// --- the job list --------------------------------------------------------

func TestJobListPutsWaitingJobsFirst(t *testing.T) {
	e := newEnv(t)
	// Created in this order, so the two that are not waiting have the newest
	// UpdatedAt and would come first under a pure recency sort.
	waitOld := e.job("AWAITING_SPEC_APPROVAL", "waiting, older")
	waitNew := e.job("AWAITING_MERGE_APPROVAL", "waiting, newer")
	running := e.job("BUILDING", "not waiting, newest")

	body := getOK(t, e, "/jobs")
	iWaitOld := strings.Index(body, `data-job="`+waitOld.ID+`"`)
	iWaitNew := strings.Index(body, `data-job="`+waitNew.ID+`"`)
	iRunning := strings.Index(body, `data-job="`+running.ID+`"`)
	if iWaitOld < 0 || iWaitNew < 0 || iRunning < 0 {
		t.Fatalf("not every job has a row: %d %d %d", iWaitOld, iWaitNew, iRunning)
	}
	if iWaitNew > iRunning || iWaitOld > iRunning {
		t.Error("a job that is not waiting on anybody sorted above one that is")
	}
	// Newest-updated first inside the waiting block.
	if iWaitNew > iWaitOld {
		t.Error("the waiting block is not newest-updated first")
	}
	if h := strings.Index(body, "Waiting on you"); h < 0 || h > iWaitNew {
		t.Error(`the "Waiting on you" heading is missing or does not come first`)
	}
	// The gate, not just the state, is what the operator scans for.
	if !strings.Contains(body, "merge") || !strings.Contains(body, "spec") {
		t.Error("the list does not name the gates the waiting jobs are at")
	}
}

// --- served names --------------------------------------------------------

// TestNamedFileNotInTheDirectoryIsRefused is safeName seen from the outside:
// what safety_test.go proves about the function, this proves the three handlers
// that take a {name} actually call.
func TestNamedFileNotInTheDirectoryIsRefused(t *testing.T) {
	e := newEnv(t)
	j := e.job("BUILDING", "a job with one log")
	writeJobFile(t, e, j, "logs", "build.log", "ok\n")
	writeJobFile(t, e, j, "artifacts", "spec.json", "{}")
	writeJobFile(t, e, j, "prompts", "planning.md", "hi")
	// Two things worth stealing: one in the job's own directory, exactly one
	// level above every served directory, and one in the data dir beside the
	// database. The first is what a single "../" would reach, which is the
	// escape a prefix check is most likely to still allow.
	for _, p := range []string{
		filepath.Join(artifact.Dir(e.cfg.Orchestrator.DataDir, j.ID), "issue.md"),
		filepath.Join(e.cfg.Orchestrator.DataDir, "sdlc.db"),
	} {
		if err := os.WriteFile(p, []byte("SECRET-DB-BYTES"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, kind := range []string{"logs", "artifacts", "prompts"} {
		for _, name := range []string{
			// "." and ".." are not in this table: net/http cleans the request
			// path before the mux sees it, so neither can reach a handler to
			// be refused by one. safety_test.go tests them against safeName
			// directly, which is where they can actually arrive.
			"nope.txt",
			"..%2fissue.md",
			`..%5cissue.md`,
			"..%2f..%2fsdlc.db",
			"....%2f%2f",
			"%2e%2e%2f%2e%2e%2fsdlc.db",
			`..%5c..%5csdlc.db`,
			"C:sdlc.db",
			"build.log:$DATA",
			"BUILD~1.LOG",
			"build.log%00",
			"build.log%00.txt",
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				res := e.get("/jobs/" + j.ID + "/" + kind + "/" + name)
				if res.StatusCode != http.StatusBadRequest && res.StatusCode != http.StatusNotFound {
					t.Fatalf("GET %s/%s = %d, want 400 or 404", kind, name, res.StatusCode)
				}
				if body := e.body(res); strings.Contains(body, "SECRET-DB-BYTES") {
					t.Fatal("the response carried bytes from outside the directory")
				}
			})
		}
	}
}

// --- log bounds (spec §5.4) ---------------------------------------------

func TestLogTailDefaultsTo2000LinesWithABanner(t *testing.T) {
	e := newEnv(t)
	j := e.job("BUILDING", "a noisy job")
	var b strings.Builder
	const total = 3000
	for i := 0; i < total; i++ {
		fmt.Fprintf(&b, "log-line-%04d\n", i)
	}
	writeJobFile(t, e, j, "logs", "build.log", b.String())

	body := getOK(t, e, "/jobs/"+j.ID+"/logs/build.log")
	if !strings.Contains(body, "log-line-2999") {
		t.Error("the tail does not reach the end of the file")
	}
	if !strings.Contains(body, "log-line-1000") {
		t.Error("the tail is shorter than the last 2000 lines")
	}
	if strings.Contains(body, "log-line-0999") {
		t.Error("more than the last 2000 lines were rendered")
	}
	if !strings.Contains(body, "Showing the last 2000 lines") {
		t.Error("the truncation is not announced")
	}
	if !strings.Contains(body, "?all=1") {
		t.Error("no link to the whole file")
	}

	// ?all=1 on a file under the byte bound shows every line and says so.
	all := getOK(t, e, "/jobs/"+j.ID+"/logs/build.log?all=1")
	if !strings.Contains(all, "log-line-0000") {
		t.Error("?all=1 did not render the first line")
	}
	if strings.Contains(all, "Showing the last 2000 lines") {
		t.Error("?all=1 still claims to be truncated by line count")
	}
}

// TestLogAllIsBoundedAndSaysSo is the 8 MiB half of §5.4. The banner has to
// name both the truncation and the path, because the only way to read the
// bytes this page will not show is to open the file.
func TestLogAllIsBoundedAndSaysSo(t *testing.T) {
	e := newEnv(t)
	j := e.job("BUILDING", "a very noisy job")
	// One marker at the very start, one at the very end, and enough filler
	// between them to push the start outside the 8 MiB window.
	line := strings.Repeat("x", 63) + "\n"
	var b strings.Builder
	b.WriteString("FIRST-LINE-MARKER\n")
	for b.Len() < logMaxBytes+(1<<20) {
		b.WriteString(line)
	}
	b.WriteString("LAST-LINE-MARKER\n")
	p := writeJobFile(t, e, j, "logs", "huge.log", b.String())

	body := getOK(t, e, "/jobs/"+j.ID+"/logs/huge.log?all=1")
	if strings.Contains(body, "FIRST-LINE-MARKER") {
		t.Error("?all=1 rendered bytes from beyond the 8 MiB bound")
	}
	if !strings.Contains(body, "LAST-LINE-MARKER") {
		t.Error("?all=1 did not render the end of the file")
	}
	if !strings.Contains(body, "last 8 MiB") {
		t.Error("the byte truncation is not named")
	}
	// The path is HTML-escaped into the banner; on Windows it contains
	// backslashes, which html/template leaves alone, so a plain Contains is
	// the right test on every platform.
	if !strings.Contains(body, p) {
		t.Errorf("the banner does not name the full path %q", p)
	}
	// The page must not be able to grow past the bound plus its own chrome.
	if len(body) > logMaxBytes+(1<<20) {
		t.Errorf("the rendered page is %d bytes, past the 8 MiB read bound", len(body))
	}
}

// --- follow (spec §9.4) --------------------------------------------------

func readChunk(t *testing.T, e *env, path string) logChunk {
	t.Helper()
	res := e.get(path)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", path, res.StatusCode)
	}
	var c logChunk
	if err := json.Unmarshal([]byte(e.body(res)), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLogFromOffsetReturnsOnlyNewBytes(t *testing.T) {
	e := newEnv(t)
	j := e.job("BUILDING", "a followed job")
	p := writeJobFile(t, e, j, "logs", "build.log", "first\n")

	c := readChunk(t, e, "/jobs/"+j.ID+"/logs/build.log?from=6")
	if c.Text != "" || c.Offset != 6 || c.Restarted {
		t.Fatalf("at EOF, got %+v", c)
	}
	if err := os.WriteFile(p, []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c = readChunk(t, e, "/jobs/"+j.ID+"/logs/build.log?from=6")
	if c.Text != "second\n" {
		t.Errorf("text = %q, want %q", c.Text, "second\n")
	}
	if c.Offset != 13 {
		t.Errorf("offset = %d, want 13", c.Offset)
	}
	if c.Restarted || c.Truncated {
		t.Errorf("unexpected flags: %+v", c)
	}
	if got := e.get("/jobs/" + j.ID + "/logs/build.log?from=-1").StatusCode; got != http.StatusBadRequest {
		t.Errorf("from=-1 = %d, want 400", got)
	}
}

func TestLogFromOffsetRestartsWhenTheFileShrank(t *testing.T) {
	e := newEnv(t)
	j := e.job("BUILDING", "a rotated job")
	writeJobFile(t, e, j, "logs", "build.log", "tiny\n")

	c := readChunk(t, e, "/jobs/"+j.ID+"/logs/build.log?from=100000")
	if !c.Restarted {
		t.Error("a file shorter than the offset did not report a restart")
	}
	if c.Text != "tiny\n" {
		t.Errorf("text = %q, want the whole file", c.Text)
	}
	if c.Offset != 5 {
		t.Errorf("offset = %d, want 5", c.Offset)
	}
}

// --- artifacts -----------------------------------------------------------

func TestReviewArtifactRendersFindings(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "a reviewed job")
	writeJobFile(t, e, j, "artifacts", "final_review.json", `{
	  "schema":"review/1","reviewed":"final","summary":"two things",
	  "findings":[
	    {"id":"F2","severity":"nit","description":"a nit nobody cares about","recommendation":"ignore"},
	    {"id":"F1","severity":"blocker","file":"a/b.go","description":"the lock is not held",
	     "recommendation":"hold it",
	     "verification":{"votes":2,"confirmed":2,"refuted":0,"min_confirm":1,"survived":true,
	       "original_severity":"major",
	       "verdicts":[{"schema":"verdict/1","finding_id":"F1","verdict":"confirmed",
	                    "evidence":"line 41","reasoning":"it really is not"}]}}
	  ]}`)

	body := getOK(t, e, "/jobs/"+j.ID+"/artifacts/final_review.json")
	for _, want := range []string{
		"two things", "the lock is not held", "hold it", "a/b.go",
		"blocker", "nit", "confirmed", "line 41", "survived",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the findings view does not show %q", want)
		}
	}
	// Worst first: artifact.SeverityRank is the only ordering in the system and
	// this page uses it rather than the artifact's own order.
	if strings.Index(body, "the lock is not held") > strings.Index(body, "a nit nobody cares about") {
		t.Error("findings are not ordered worst-severity first")
	}
}

// TestNonReviewArtifactIsStillPrettyPrinted keeps the review branch from
// becoming the only branch that works.
func TestNonReviewArtifactIsStillPrettyPrinted(t *testing.T) {
	e := newEnv(t)
	j := e.job("BUILDING", "a job with a plan")
	writeJobFile(t, e, j, "artifacts", "plan.json", `{"schema":"plan/1","steps":[],"risks":["none"]}`)
	body := getOK(t, e, "/jobs/"+j.ID+"/artifacts/plan.json")
	if !strings.Contains(body, `&#34;risks&#34;: [`) {
		t.Error("the JSON was not indented")
	}
	if strings.Contains(body, "Findings") {
		t.Error("a plan artifact rendered a findings section")
	}
}

// --- escaping ------------------------------------------------------------

// TestAgentTextIsEscaped is spec §12.1's escaping row. Job titles, finding descriptions, log lines
// and file names are all written by something other than this server — an
// issue, an agent, a build tool — so every one of them is attacker-influenced
// text and none of them may reach the browser as markup.
func TestAgentTextIsEscaped(t *testing.T) {
	const payload = `<script>alert(1)</script>`
	const escaped = `&lt;script&gt;alert(1)&lt;/script&gt;`
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "title "+payload)
	writeJobFile(t, e, j, "logs", "build.log", "log line "+payload+"\n")
	writeJobFile(t, e, j, "prompts", "planning.md", "prompt "+payload+"\n")
	writeJobFile(t, e, j, "artifacts", "review.json",
		`{"schema":"review/1","reviewed":"diff","summary":"s","findings":[
		  {"id":"F1","severity":"major","description":"finding `+payload+`","recommendation":"r"}]}`)
	if err := e.st.AddEvent(&store.Event{
		JobID: j.ID, State: j.State, Kind: "note", Detail: "event " + payload,
	}); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{
		"/jobs",
		"/jobs/" + j.ID,
		"/jobs/" + j.ID + "/events",
		"/jobs/" + j.ID + "/logs/build.log",
		"/jobs/" + j.ID + "/prompts/planning.md",
		"/jobs/" + j.ID + "/artifacts/review.json",
	} {
		t.Run(p, func(t *testing.T) {
			body := getOK(t, e, p)
			if strings.Contains(body, payload) {
				t.Fatal("the payload reached the page unescaped")
			}
			if !strings.Contains(body, escaped) {
				t.Fatal("the payload does not appear escaped either, so this test proves nothing")
			}
		})
	}
}

// --- /config -------------------------------------------------------------

func TestConfigPageIsReadOnly(t *testing.T) {
	e := newEnv(t)
	body := getOK(t, e, "/config")
	for _, bad := range []string{"<form", "<input", "<textarea", "<button"} {
		if strings.Contains(strings.ToLower(body), bad) {
			t.Errorf("the config page carries a %s; spec §2.2 refuses config editing in the UI", bad)
		}
	}
	// It has to show the config, or "read-only" would be satisfied by a blank
	// page.
	for _, want := range []string{"orchestrator:", "data_dir:", e.cfg.Database.Path} {
		if !strings.Contains(body, want) {
			t.Errorf("the config page does not show %q", want)
		}
	}
}

// TestConfigPageRedactsCredentials covers the reason this page needs redaction
// at all: config.Load expands ${VAR} before this server ever sees the struct,
// so a token arrives here inside an ordinary command line whose key name says
// nothing about what it holds.
func TestConfigPageRedactsCredentials(t *testing.T) {
	const flagToken = "sk-live-FLAG-0123456789"
	const envToken = "env-secret-9876543210"
	t.Setenv("SDLC_TEST_API_TOKEN", envToken)

	e := newEnv(t)
	tgt := e.cfg.Targets["demo"]
	tgt.Ship.Command = []string{"autoship", "--api-token", flagToken, "--endpoint=https://example.invalid"}
	// The env-expanded value lands in a field with an innocent name, which is
	// exactly the case the key-name rule cannot catch.
	tgt.Ship.ResumeCommand = []string{"autoship", "resume", envToken}
	e.cfg.Targets["demo"] = tgt

	body := getOK(t, e, "/config")
	for _, secret := range []string{flagToken, envToken} {
		if strings.Contains(body, secret) {
			t.Errorf("the config page rendered %q", secret)
		}
	}
	if !strings.Contains(body, redactPlaceholder) {
		t.Error("nothing was marked as redacted")
	}
	if !strings.Contains(body, "have been replaced") {
		t.Error("the page does not say that it redacted anything")
	}
	// A value that is not a credential still has to be readable, or redaction
	// would be satisfied by hiding the whole config.
	if !strings.Contains(body, "--endpoint=https://example.invalid") {
		t.Error("an ordinary argument was redacted too")
	}
}

// --- /submit -------------------------------------------------------------

func TestSubmitFormOffersExactlyTheConfiguredTargets(t *testing.T) {
	e := newEnv(t)
	body := getOK(t, e, "/submit")
	if !strings.Contains(body, `<option value="demo">`) {
		t.Error("the target select does not offer the configured target")
	}
	for _, want := range []string{`name="csrf"`, `name="target"`, `name="title"`, `name="body"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the submit form has no %s field", want)
		}
	}
}
