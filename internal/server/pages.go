package server

// Read-only pages (spec §5.1). Owner: W2-H.
//
// Nothing here derives anything. The job rows come from store, the artifacts
// come from internal/artifact, and the gate a job is waiting at comes from
// review.GateFor — spec §6 says there is one of each of those and this file is
// not allowed to become a second one.
//
// Every file these handlers open by a name taken from the URL goes through
// safeName. That is not defence in depth, it is the only check: the log,
// artifact and prompt routes each interpolate a path segment an attacker picks
// into a filesystem path, and safety.go explains why membership in the
// directory listing is the only test that holds on every platform.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// The bounds of spec §5.4 and §9.4, in one place because three handlers share
// them and a bound that is written out twice is a bound that gets changed once.
const (
	// logTailLines is what the log page shows without ?all=1.
	logTailLines = 2000
	// logMaxBytes is the most any single page will read from a file on disk.
	// It is read from the tail, not the head: the end of a log is the part
	// that says what went wrong.
	logMaxBytes = 8 << 20
	// logFollowBytes bounds one ?from= response (spec §9.4). Smaller than
	// logMaxBytes because a follow poll runs every two seconds.
	logFollowBytes = 1 << 20
)

// --- /jobs ---------------------------------------------------------------

// jobsPage splits the list rather than sorting one list by a computed key,
// because "waiting on you" is not a rank — it is the reason the page exists,
// and a job that needs a decision must not be able to sort below one that does
// not just because the other was touched more recently.
type jobsPage struct {
	Waiting []jobRow
	Rest    []jobRow
	// Counts is the one-line summary above the grid. It counts every job in
	// the database, not the filtered rows below it, because it is what the
	// filter chips are counting *down from* — a chip labelled "failed 2" that
	// read 0 whenever another filter was active would be useless.
	Counts jobCounts
	// Show is the active filter, "" for the default. It is a query parameter
	// rather than a class app.js toggles: a filtered list is a list an
	// operator will link someone else to, and it survives a reload.
	Show    string
	Filters []jobFilter
}

// jobFilter is one chip. Count comes from the same pass that built the rows,
// so a chip never offers a filter with nothing behind it without saying so.
type jobFilter struct {
	Key   string
	Label string
	Count int
	On    bool
}

// jobCounts is the header line's arithmetic. Open is every job the engine
// could still act on, so it deliberately excludes the terminal three; a "11
// open" that counted a job cancelled last Tuesday would make the number
// useless for deciding whether to start another.
type jobCounts struct {
	Total   int
	Open    int
	Waiting int
	Running int
	// Blocked is the engine's own backoff — a job rate-limited out of its
	// backend and sleeping on a timer. It is neither running (no work is
	// dispatched for it) nor waiting (nobody is being asked to decide
	// anything), and counting it as either misreports what the operator can
	// act on.
	Blocked int
	Held    int
	Failed  int
	Done    int
}

type jobRow struct {
	Job  *store.Job
	Gate string
	// The three shapes the grid draws, all from shape.go so the bar, the state
	// colour and the pipeline strip on the job's own page cannot disagree.
	Pct        int
	StateClass string
	GateClass  string
}

// newJobRow shapes one row. Every page that lists jobs goes through it — the
// grid, and the review queue rail — so a job looks the same in both.
func newJobRow(j *store.Job) jobRow {
	g := review.GateFor(j.State)
	return jobRow{
		Job:        j,
		Gate:       g,
		Pct:        phasePct(j.State, j.PrevState),
		StateClass: stateClass(j.State),
		GateClass:  gateClass(g),
	}
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.st.ListJobs()
	if err != nil {
		s.log.Printf("listing jobs: %v", err)
		s.fail(w, r, http.StatusInternalServerError, "the job list could not be read from the database")
		return
	}
	// Newest-updated first, before the split, so both blocks inherit the order.
	sort.SliceStable(jobs, func(i, k int) bool { return jobs[i].UpdatedAt.After(jobs[k].UpdatedAt) })
	body := jobsPage{Show: r.URL.Query().Get("show")}
	for _, j := range jobs {
		row := newJobRow(j)
		body.Counts.Total++
		switch {
		case j.State == "COMPLETED":
			body.Counts.Done++
		case isTerminalState(j.State):
			body.Counts.Failed++
		case row.Gate == review.GateHold:
			body.Counts.Open++
			body.Counts.Held++
		case row.Gate != "":
			body.Counts.Open++
			body.Counts.Waiting++
		case isBackoffState(j.State):
			body.Counts.Open++
			body.Counts.Blocked++
		default:
			body.Counts.Open++
			body.Counts.Running++
		}
		if !showsJob(body.Show, row) {
			continue
		}
		if row.Gate != "" {
			body.Waiting = append(body.Waiting, row)
			continue
		}
		body.Rest = append(body.Rest, row)
	}
	body.Filters = jobFilters(body.Show, body.Counts)
	s.render(w, r, "jobs.html", s.page(r, "Jobs", "jobs", body))
}

// showsJob applies the ?show= filter. The default hides the terminal three,
// which is the only filtering this page has ever done implicitly and is now
// named: an operator who wants a cancelled job can ask for one.
func showsJob(show string, row jobRow) bool {
	state := row.Job.State
	switch show {
	case "all":
		return true
	case "waiting":
		return row.Gate != "" && row.Gate != review.GateHold
	case "running":
		return row.Gate == "" && isRunning(state)
	case "blocked":
		return isBackoffState(state)
	case "held":
		return row.Gate == review.GateHold
	case "failed":
		return isTerminalState(state) && state != "COMPLETED"
	case "done":
		return state == "COMPLETED"
	default:
		return !isTerminalState(state)
	}
}

func jobFilters(show string, c jobCounts) []jobFilter {
	on := func(k string) bool {
		if k == "open" {
			return show == "" || show == "open"
		}
		return show == k
	}
	return []jobFilter{
		{Key: "open", Label: "open", Count: c.Open, On: on("open")},
		{Key: "waiting", Label: "waiting", Count: c.Waiting, On: on("waiting")},
		{Key: "running", Label: "running", Count: c.Running, On: on("running")},
		{Key: "blocked", Label: "blocked", Count: c.Blocked, On: on("blocked")},
		{Key: "held", Label: "held", Count: c.Held, On: on("held")},
		{Key: "failed", Label: "failed", Count: c.Failed, On: on("failed")},
		{Key: "done", Label: "done", Count: c.Done, On: on("done")},
		{Key: "all", Label: "all", Count: c.Total, On: on("all")},
	}
}

// --- /jobs/{id} ----------------------------------------------------------

type jobPage struct {
	Job    *store.Job
	Gate   string
	Target config.Target
	// LastProgress is the most recent progress event, nil when the job has
	// never emitted one. A state that has been running for eight minutes
	// should say what it is running; without it, "in flight" and "hung" look
	// identical from here, which is the same reasoning cli.printLastProgress
	// carries.
	LastProgress *store.Event
	// Progress is LastProgress's detail column, indented when it is JSON. The
	// event is kept alongside it because the page shows the time and the state
	// it was emitted in, which the detail does not carry.
	Progress string
	Events   int
	// Form is the model _forms.html's named templates render from (actions.go).
	// The page carries it rather than building the forms itself: the hidden
	// state field and the action URL have to describe the same job, and one
	// model is how that is guaranteed.
	Form         FormModel
	ArtifactsDir string
	LogsDir      string
	PromptsDir   string
	// Pipeline is the same strip the gate page draws, from shape.go. The job
	// page is where an operator lands when a job is *not* at a gate, and "how
	// far has this got" is the question they arrived with.
	Pipeline   []phaseView
	StateClass string
	GateClass  string
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	t, ok := s.target(w, r, j)
	if !ok {
		return
	}
	dir := s.cfg.Get().Orchestrator.DataDir
	body := jobPage{
		Job:          j,
		Gate:         review.GateFor(j.State),
		Target:       t,
		Pipeline:     pipelineFor(j.State, j.PrevState),
		StateClass:   stateClass(j.State),
		GateClass:    gateClass(review.GateFor(j.State)),
		Form:         s.formModel(r, j, ""),
		ArtifactsDir: artifact.ArtifactsDir(dir, j.ID),
		LogsDir:      artifact.LogsDir(dir, j.ID),
		PromptsDir:   artifact.PromptsDir(dir, j.ID),
	}
	// Two narrow queries rather than one read of the whole history: this page
	// wants a count and the newest progress line, and app.js reloads it on a
	// timer for as long as the job is live, so a job with hundreds of events
	// would otherwise re-read and re-unmarshal every detail blob it has ever
	// written, every ten seconds, to render one number and one line.
	//
	// Neither failure is fatal: the timeline is a detail of this page, and
	// refusing to render the state, the hold reason and the links because one
	// query failed would withhold exactly what the operator came for.
	if n, err := s.st.CountEvents(j.ID); err == nil {
		body.Events = n
	} else {
		s.log.Printf("counting events for %s: %v", j.ID, err)
	}
	if ev, err := s.st.LastEventOfKind(j.ID, "progress"); err != nil {
		s.log.Printf("reading the last progress event for %s: %v", j.ID, err)
	} else if ev != nil {
		body.LastProgress = ev
		body.Progress = prettyJSON(ev.Detail)
	}
	s.render(w, r, "job.html", s.page(r, j.ID, "jobs", body))
}

// --- /jobs/{id}/events ---------------------------------------------------

type eventsPage struct {
	Job    *store.Job
	Events []eventRow
	Kinds  []string
	States []string
	Kind   string // the active ?kind= filter, "" for all
	State  string // the active ?state= filter, "" for all
	Total  int    // before filtering
}

type eventRow struct {
	Event *store.Event
	// Detail is the event's detail column, indented when it parses as JSON and
	// left exactly as stored when it does not. Nothing is dropped either way:
	// this page is the record of what happened.
	Detail string
	Exit   string
	Dur    time.Duration
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	evs, err := s.st.ListEvents(j.ID)
	if err != nil {
		s.log.Printf("reading events for %s: %v", j.ID, err)
		s.fail(w, r, http.StatusInternalServerError, "the event timeline could not be read from the database")
		return
	}
	body := eventsPage{
		Job:   j,
		Kind:  r.URL.Query().Get("kind"),
		State: r.URL.Query().Get("state"),
		Total: len(evs),
	}
	seenKind, seenState := map[string]bool{}, map[string]bool{}
	for _, e := range evs {
		// The filter menus list what this job actually produced rather than
		// every kind the schema allows, so a filter offered here always has at
		// least one row behind it.
		if !seenKind[e.Kind] {
			seenKind[e.Kind] = true
			body.Kinds = append(body.Kinds, e.Kind)
		}
		if !seenState[e.State] {
			seenState[e.State] = true
			body.States = append(body.States, e.State)
		}
		if body.Kind != "" && e.Kind != body.Kind {
			continue
		}
		if body.State != "" && e.State != body.State {
			continue
		}
		row := eventRow{Event: e, Detail: prettyJSON(e.Detail), Dur: time.Duration(e.DurationMS) * time.Millisecond}
		if e.ExitCode.Valid {
			row.Exit = strconv.FormatInt(e.ExitCode.Int64, 10)
		}
		body.Events = append(body.Events, row)
	}
	sort.Strings(body.Kinds)
	sort.Strings(body.States)
	s.render(w, r, "events.html", s.page(r, j.ID+" events", "jobs", body))
}

// prettyJSON indents s when it is JSON and returns it untouched otherwise. The
// detail column holds whatever the emitting call site put there, so "it is
// always JSON" is not a property this page may assume.
func prettyJSON(s string) string {
	t := strings.TrimSpace(s)
	if t == "" || !json.Valid([]byte(t)) {
		return s
	}
	var buf strings.Builder
	if err := indentJSON(&buf, t); err != nil {
		return s
	}
	return buf.String()
}

func indentJSON(w io.Writer, s string) error {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// Struct-free re-encoding sorts object keys, which is what makes two
	// renderings of the same detail comparable by eye.
	return enc.Encode(v)
}

// --- directory listings --------------------------------------------------

type listPage struct {
	Job   *store.Job
	Dir   string
	Kind  string // "logs" | "artifacts", used for the link prefix
	Files []fileRow
	// Missing distinguishes "this job has produced nothing yet" from "the
	// directory is there and empty". Both are normal; only the first is normal
	// for a job that has not run.
	Missing bool
}

type fileRow struct {
	Name    string
	Href    string
	Size    int64
	ModTime time.Time
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.serveListing(w, r, "logs")
}

func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	s.serveListing(w, r, "artifacts")
}

func (s *Server) serveListing(w http.ResponseWriter, r *http.Request, kind string) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	dir := artifact.LogsDir(s.cfg.Get().Orchestrator.DataDir, j.ID)
	title := j.ID + " logs"
	if kind == "artifacts" {
		dir = artifact.ArtifactsDir(s.cfg.Get().Orchestrator.DataDir, j.ID)
		title = j.ID + " artifacts"
	}
	body := listPage{Job: j, Dir: dir, Kind: kind}
	ents, err := os.ReadDir(dir)
	if err != nil {
		// A job created a minute ago has no directory yet, and answering 404
		// for that would say the job does not exist. The page renders empty and
		// says which directory it looked in.
		body.Missing = true
		s.render(w, r, kind+".html", s.page(r, title, "jobs", body))
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		body.Files = append(body.Files, fileRow{
			Name: e.Name(),
			// PathEscape, not raw interpolation: html/template escapes a value
			// for the href context but will not turn a '?' or a '#' in a
			// filename into path bytes, and an artifact named "a?b.json" would
			// otherwise link to a query string.
			Href:    "/jobs/" + url.PathEscape(j.ID) + "/" + kind + "/" + url.PathEscape(e.Name()),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	// Newest last, matching `sdlc logs`: the file the running state is writing
	// is the one the operator wants, and it is the one at the bottom.
	sort.SliceStable(body.Files, func(a, b int) bool {
		if body.Files[a].ModTime.Equal(body.Files[b].ModTime) {
			return body.Files[a].Name < body.Files[b].Name
		}
		return body.Files[a].ModTime.Before(body.Files[b].ModTime)
	})
	s.render(w, r, kind+".html", s.page(r, title, "jobs", body))
}

// --- one named file ------------------------------------------------------

// resolveNamed turns {id} and {name} into a path inside dir, answering for the
// caller when either fails. The two errors are answered differently on purpose
// (see safety.go): a name that may not be asked for is a 400, and a name that
// is simply not there is a 404, because a 404 for a traversal attempt would
// tell the caller that some other spelling might work.
func (s *Server) resolveNamed(w http.ResponseWriter, r *http.Request, dirFor func(dataDir, jobID string) string) (*store.Job, string, string, bool) {
	j, ok := s.job(w, r)
	if !ok {
		return nil, "", "", false
	}
	name := r.PathValue("name")
	p, err := safeName(dirFor(s.cfg.Get().Orchestrator.DataDir, j.ID), name)
	if err != nil {
		if errors.Is(err, errUnsafeName) {
			s.fail(w, r, http.StatusBadRequest, "that is not a name this server will serve")
			return nil, "", "", false
		}
		s.fail(w, r, http.StatusNotFound, fmt.Sprintf("%s has no file named %q here", j.ID, name))
		return nil, "", "", false
	}
	return j, name, p, true
}

type logPage struct {
	Job  *store.Job
	Name string
	// Path is the full path on disk, named on the page because §5.4 requires a
	// truncated view to say where the untruncated bytes are.
	Path    string
	Size    int64
	ModTime time.Time
	Text    string
	All     bool
	// Offset is where Text ends in the file, which is what the follow poller
	// starts from. Text is always read to EOF, so it is the size at read time.
	Offset int64
	// LineTruncated is set when the default view dropped older lines;
	// ByteTruncated when the 8 MiB read bound did.
	LineTruncated bool
	ByteTruncated bool
	Lines         int
	// TotalLines is meaningful only when the whole file was read; the template
	// says nothing about a total it does not have.
	TotalLines int
	FollowURL  string
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	j, name, p, ok := s.resolveNamed(w, r, artifact.LogsDir)
	if !ok {
		return
	}
	if from := r.URL.Query().Get("from"); from != "" {
		s.serveLogFrom(w, r, p, from)
		return
	}
	all := r.URL.Query().Get("all") == "1"
	data, size, modTime, byteTrunc, err := readTail(p, logMaxBytes)
	if err != nil {
		s.log.Printf("reading log %s: %v", p, err)
		s.fail(w, r, http.StatusInternalServerError, "that log could not be read")
		return
	}
	body := logPage{
		Job: j, Name: name, Path: p, Size: size, ModTime: modTime,
		All: all, Offset: size, ByteTruncated: byteTrunc,
		FollowURL: "/jobs/" + url.PathEscape(j.ID) + "/logs/" + url.PathEscape(name),
	}
	text := string(data)
	lines := splitLines(text)
	if !byteTrunc {
		body.TotalLines = len(lines)
	}
	if !all && len(lines) > logTailLines {
		lines = lines[len(lines)-logTailLines:]
		body.LineTruncated = true
	}
	body.Lines = len(lines)
	body.Text = strings.Join(lines, "\n")
	s.render(w, r, "log.html", s.page(r, j.ID+" "+name, "jobs", body))
}

// logChunk is the follow response (spec §9.4). It is JSON rather than plain
// bytes because the poller needs two things back — the new bytes and the offset
// to ask from next — and a header carrying the second would be a second place
// to get the encoding wrong.
type logChunk struct {
	Offset int64  `json:"offset"`
	Text   string `json:"text"`
	// Restarted reports that the file is now shorter than the offset asked
	// for, so these bytes are the file from 0 rather than a continuation. A
	// poller that appended them without noticing would show the file twice.
	Restarted bool `json:"restarted"`
	// Truncated reports that more than one response worth of bytes was
	// waiting; the poller catches up by asking again from Offset.
	Truncated bool `json:"truncated"`
}

func (s *Server) serveLogFrom(w http.ResponseWriter, r *http.Request, p, from string) {
	off, err := strconv.ParseInt(from, 10, 64)
	if err != nil || off < 0 {
		s.fail(w, r, http.StatusBadRequest, "from must be a byte offset")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		s.log.Printf("opening log %s: %v", p, err)
		s.fail(w, r, http.StatusInternalServerError, "that log could not be read")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.log.Printf("stat log %s: %v", p, err)
		s.fail(w, r, http.StatusInternalServerError, "that log could not be read")
		return
	}
	chunk := logChunk{Offset: off}
	size := info.Size()
	if off > size {
		// The file shrank — it was rotated or rewritten under the poller. The
		// bytes at the old offset are not a continuation of what the page is
		// already showing, so the response says so and starts again from 0.
		off, chunk.Restarted = 0, true
	}
	n := size - off
	if n > logFollowBytes {
		n, chunk.Truncated = logFollowBytes, true
	}
	if n > 0 {
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
			s.log.Printf("reading log %s at %d: %v", p, off, err)
			s.fail(w, r, http.StatusInternalServerError, "that log could not be read")
			return
		}
		chunk.Text = string(buf)
	}
	chunk.Offset = off + n
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// A followed log is the one thing on this server that is never the same
	// twice; a cached response would freeze the tail the operator is watching.
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(chunk); err != nil {
		s.log.Printf("writing log chunk for %s: %v", p, err)
	}
}

// readTail returns at most max bytes from the end of the file at p, reporting
// whether the file was longer than that. The tail rather than the head because
// a truncated log is read for what happened last.
func readTail(p string, max int64) (data []byte, size int64, mod time.Time, truncated bool, err error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, time.Time{}, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, time.Time{}, false, err
	}
	size = info.Size()
	mod = info.ModTime()
	off := int64(0)
	n := size
	if n > max {
		off, n, truncated = size-max, max, true
	}
	buf := make([]byte, n)
	if n > 0 {
		if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
			return nil, 0, time.Time{}, false, err
		}
	}
	if truncated {
		// The cut landed mid-line. Showing half a line as if it were a whole
		// one is how a reader gets a wrong idea of what a log said, so the
		// partial first line is dropped and the banner accounts for it.
		if i := indexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return buf, size, mod, truncated, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// splitLines splits on newlines and drops the empty element a trailing newline
// produces, so a 2000-line file does not report 2001 lines.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// --- artifacts and prompts ----------------------------------------------

type artifactPage struct {
	Job     *store.Job
	Name    string
	Path    string
	Size    int64
	Pretty  string
	IsJSON  bool
	Trimmed bool
	// Review and Findings are set only for a review/1 artifact. That is the
	// shape a human reads a review in — severity first, then whether the
	// verifiers upheld it — and a pretty-printed object is not that.
	Review   *artifact.Review
	Findings []artifact.Finding
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	j, name, p, ok := s.resolveNamed(w, r, artifact.ArtifactsDir)
	if !ok {
		return
	}
	raw, size, trimmed, err := readHead(p, logMaxBytes)
	if err != nil {
		s.log.Printf("reading artifact %s: %v", p, err)
		s.fail(w, r, http.StatusInternalServerError, "that artifact could not be read")
		return
	}
	body := artifactPage{Job: j, Name: name, Path: p, Size: size, Trimmed: trimmed}
	body.Pretty = string(raw)
	if !trimmed && json.Valid(raw) {
		body.IsJSON = true
		body.Pretty = prettyJSON(string(raw))
	}
	// The schema is read off the file rather than inferred from the name: the
	// engine writes review artifacts under several names (review.json,
	// final_review.json, review_2.json), and a name test would silently stop
	// recognising the next one.
	if body.IsJSON && schemaOf(raw) == "review/1" {
		// LoadReview validates; an artifact that does not pass validation is
		// still shown as JSON rather than not shown at all, because a
		// malformed artifact is exactly what an operator is looking at this
		// page to see.
		if rev, err := artifact.LoadReview(p); err == nil {
			body.Review = rev
			body.Findings = sortFindings(rev.Findings)
		}
	}
	s.render(w, r, "artifact.html", s.page(r, j.ID+" "+name, "jobs", body))
}

// schemaOf reads just the schema discriminator, so a file that is JSON but not
// one of ours costs one small decode rather than a failed typed unmarshal.
func schemaOf(raw []byte) string {
	var probe struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Schema
}

// sortFindings orders by severity, worst first, using artifact.SeverityRank —
// spec §6 says that map is the only severity ordering in the system. Ties keep
// the artifact's own order, which is the order the reviewer wrote them in.
func sortFindings(in []artifact.Finding) []artifact.Finding {
	out := make([]artifact.Finding, len(in))
	copy(out, in)
	sort.SliceStable(out, func(a, b int) bool {
		return artifact.SeverityRank[out[a].Severity] > artifact.SeverityRank[out[b].Severity]
	})
	return out
}

type promptPage struct {
	Job     *store.Job
	Name    string
	Path    string
	Size    int64
	Text    string
	Trimmed bool
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	j, name, p, ok := s.resolveNamed(w, r, artifact.PromptsDir)
	if !ok {
		return
	}
	raw, size, trimmed, err := readHead(p, logMaxBytes)
	if err != nil {
		s.log.Printf("reading prompt %s: %v", p, err)
		s.fail(w, r, http.StatusInternalServerError, "that prompt could not be read")
		return
	}
	s.render(w, r, "prompt.html", s.page(r, j.ID+" "+name, "jobs",
		promptPage{Job: j, Name: name, Path: p, Size: size, Text: string(raw), Trimmed: trimmed}))
}

// readHead returns at most max bytes from the start of p. Artifacts and prompts
// are read from the front because they are documents, not logs: their first
// bytes are the ones that say what they are.
func readHead(p string, max int64) (data []byte, size int64, truncated bool, err error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	size = info.Size()
	data, err = io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return nil, 0, false, err
	}
	return data, size, size > max, nil
}

// --- /submit -------------------------------------------------------------

type submitPage struct {
	Targets []submitTarget
	// Gates, Roles and Budgets are what the loaded config will do to this job,
	// shown beside the form and not editable in it. They are context, not
	// input: `sdlc submit` takes a target, a title and a body, and a page that
	// offered a per-job gate list would be promising something no handler
	// honours (spec §5.2).
	Gates   []gateOpt
	Roles   []roleRow
	Budgets []kvRow
	// Steps is what happens after the button, in the order it happens. The
	// design doc asks for it and it costs nothing to be true: an operator who
	// knows a worktree is cut first knows where to look when it is not.
	Steps []string
}

type submitTarget struct {
	Name   string
	Repo   string
	Branch string
}

// gateOpt is one row of "park for approval at". On is read from the loaded
// policy, so the page says which gates are actually armed rather than which
// ones exist.
type gateOpt struct {
	Name   string
	Detail string
	On     bool
	// Always marks the two gates policy cannot switch off. A checkbox that
	// cannot be unchecked is a lie about who is in control, so these draw as
	// what they are.
	Always bool
}

type roleRow struct {
	Role    string
	Agent   string
	Backend string
	Model   string
}

type kvRow struct {
	K    string
	V    string
	Note string
}

func (s *Server) handleSubmitForm(w http.ResponseWriter, r *http.Request) {
	body := submitPage{Steps: []string{
		"A worktree is cut from the target's default branch at its current head.",
		"PLANNING writes a plan and a test list into the job's artifacts directory.",
		"The job parks at the first armed gate and appears in Waiting on you.",
		"Nothing is pushed or shipped until you approve the merge and release gates.",
	}}
	names := make([]string, 0, len(s.cfg.Get().Targets))
	for name := range s.cfg.Get().Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := s.cfg.Get().Targets[name]
		body.Targets = append(body.Targets, submitTarget{Name: name, Repo: t.RepoPath, Branch: t.DefaultBranch})
	}
	body.Gates = s.gateOpts()
	body.Roles = s.roleRows()
	body.Budgets = s.budgetRows()
	s.render(w, r, "submit.html", s.page(r, "Submit a job", "submit", body))
}

// gateOpts reports the four gates and which of them this config arms. The two
// that are always armed are listed first and marked, because the order a
// reader scans them in is the order the pipeline reaches them.
func (s *Server) gateOpts() []gateOpt {
	p := s.cfg.Get().Policies
	return []gateOpt{
		{Name: "spec", Detail: "After design review, before any code is written.", On: p.HumanGate("spec")},
		{Name: "code", Detail: "After code review, before build and test.", On: p.HumanGate("code")},
		{Name: "merge", Detail: "Before the branch lands on the base branch.", On: true, Always: true},
		{Name: "release", Detail: "Before anything leaves this machine.", On: true, Always: true},
	}
}

// roleRows resolves each agent state to the backend and model that will run
// it. The resolution is the config's own — states name an agent, agents name a
// backend — and is read rather than reimplemented, so a page that says "agy"
// is saying what the engine will actually invoke.
func (s *Server) roleRows() []roleRow {
	out := make([]roleRow, 0, len(config.AgentStates))
	for _, st := range config.AgentStates {
		row := roleRow{Role: strings.ToLower(st)}
		if cs, ok := s.cfg.Get().States[st]; ok {
			row.Agent = cs.Agent
			if a, ok := s.cfg.Get().Agents[cs.Agent]; ok {
				row.Backend, row.Model = a.Backend, a.Model
			}
		}
		out = append(out, row)
	}
	return out
}

// budgetRows is the engine's limits, not the agent's. Each one is a number the
// engine enforces on its own; naming what happens when it is reached is the
// half of a budget that tells an operator whether to raise it.
func (s *Server) budgetRows() []kvRow {
	l := s.cfg.Get().Limits
	return []kvRow{
		{K: "agent invocations", V: strconv.Itoa(l.MaxAgentInvocationsPerJob), Note: "per job"},
		{K: "fix attempts", V: strconv.Itoa(l.MaxFixAttempts), Note: "then the job fails"},
		{K: "flake retries", V: strconv.Itoa(l.MaxFlakeRetries), Note: "on an unmodified tree"},
		{K: "code review rounds", V: strconv.Itoa(l.MaxCodeReviewRounds), Note: ""},
		{K: "design review rounds", V: strconv.Itoa(l.MaxDesignReviewRounds), Note: ""},
		{K: "max job duration", V: l.MaxJobDuration.String(), Note: "then TIMED_OUT"},
	}
}

// --- /config -------------------------------------------------------------

type configPage struct {
	// Path is the config file this process loaded, "" when it ran on defaults.
	Path     string
	YAML     string
	Redacted bool
	DataDir  string
	DBPath   string
	LockFile string
	Listen   string
	JobsDir  string
	// Sections is the loaded config in the shape a reader asks questions in —
	// which backend reviews, which gates park, what the engine will stop for —
	// rather than the shape it is stored in. The YAML below it is still the
	// record; this is the index into it, and every value here is read off the
	// same *config.Config, never restated.
	Sections []configSection

	// --- the edit surface. Everything below is about RawText, never Sections
	// or YAML above: those two stay the decoded, redacted, read-only view they
	// always were. RawText is the raw bytes of the file at Path — no
	// expansion, no defaults filled in, nothing redacted — because ${VAR}
	// expansion happens on that raw text before it is ever parsed
	// (config.expandEnv, run inside config.Parse), so the file on disk is
	// meant to hold "${GITHUB_TOKEN}", never a literal secret. A box that
	// shows exactly what's on disk therefore never needs to redact anything
	// it round-trips — see config.html for the sentence that tells the
	// operator so.

	// RawText is what the editor's textarea shows: the file at Path on a
	// plain GET, the version named by a resolved ?load= on this GET, or — on
	// a refused save — the exact text just submitted, never what is still on
	// disk. Losing an edit to a typo would be actively hostile.
	RawText string
	// LoadedFrom names the history entry RawText came from ("" for the file
	// on disk or a just-submitted edit), so the page can say a load has not
	// been saved yet.
	LoadedFrom string
	// Saved is set once, right after a successful save's redirect (?saved=1).
	Saved bool
	// SaveErr is set only on a refused save: the parse/validate error, or a
	// sentence naming the fields that need a restart. "" on every other GET.
	SaveErr string
}

type configSection struct {
	Title string
	Note  string
	Rows  []kvRow
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	body, err := s.buildConfigPage()
	if err != nil {
		s.log.Printf("re-marshalling config: %v", err)
		s.fail(w, r, http.StatusInternalServerError, "the loaded config could not be re-marshalled to YAML")
		return
	}
	if name := r.URL.Query().Get("load"); name != "" && body.Path != "" {
		p, err := safeName(configHistoryDir(body.Path), name)
		if err != nil {
			if errors.Is(err, errUnsafeName) {
				s.fail(w, r, http.StatusBadRequest, "that is not a name this server will serve")
				return
			}
			s.fail(w, r, http.StatusNotFound, fmt.Sprintf("no history entry named %q", name))
			return
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			s.log.Printf("reading config history %s: %v", p, err)
			s.fail(w, r, http.StatusInternalServerError, "that version could not be read")
			return
		}
		body.RawText, body.LoadedFrom = string(raw), name
	}
	body.Saved = r.URL.Query().Get("saved") == "1"
	s.render(w, r, "config.html", s.page(r, "Config", "config", body))
}

// buildConfigPage assembles everything about the loaded config that does not
// depend on which request is asking for it — the decoded/redacted sections
// and, when a file backs this process at all, the raw bytes at Path. Both
// handleConfig and handleConfigSave's refusal path start from this and then
// set the request-specific fields (RawText on refusal is the text that was
// submitted, not what this function read from disk).
func (s *Server) buildConfigPage() (configPage, error) {
	cfg := s.cfg.Get()
	body := configPage{
		Path:     cfg.Path,
		DataDir:  absOrAsIs(cfg.Orchestrator.DataDir),
		DBPath:   absOrAsIs(cfg.Database.Path),
		LockFile: absOrAsIs(cfg.Orchestrator.LockFile),
		Listen:   cfg.Server.Listen,
		JobsDir:  absOrAsIs(filepath.Join(cfg.Orchestrator.DataDir, "jobs")),
	}
	text, redacted, err := redactedConfigYAML(cfg)
	if err != nil {
		return configPage{}, err
	}
	body.YAML, body.Redacted = text, redacted
	body.Sections = s.configSections()
	if cfg.Path != "" {
		// A file that fails to read (removed out from under the process, a
		// permissions change) leaves RawText empty rather than failing the
		// whole page: every other section here is still true and worth
		// showing, and an empty editor is the honest reflection of "this
		// could not be read" — Save against it will simply fail the same way.
		if raw, err := os.ReadFile(cfg.Path); err == nil {
			body.RawText = string(raw)
		}
	}
	return body, nil
}

// configSections shapes the loaded config into the four questions this page
// gets asked. No value is computed here that is not already in the struct: a
// summary that derived something would be a second opinion about a config the
// loader has already validated.
func (s *Server) configSections() []configSection {
	c := s.cfg.Get()
	backends := make([]string, 0, len(c.Backends))
	for name := range c.Backends {
		backends = append(backends, name)
	}
	sort.Strings(backends)

	independence := "not required"
	if st, ok := c.States[config.StFinalReview]; ok && st.MustDifferBackendFrom != "" {
		independence = "must differ from " + strings.ToLower(st.MustDifferBackendFrom)
	}
	reviewerWrites := "not checked"
	if c.Policies.ReviewerDiffMustBeEmpty {
		reviewerWrites = "reviewer diff must be empty"
	}

	var armed []string
	for _, g := range s.gateOpts() {
		if g.On {
			armed = append(armed, g.Name)
		}
	}

	verify := "off — every finding gates on the reviewer's word"
	if c.Limits.VerifyVotes > 0 {
		verify = fmt.Sprintf("%d votes, %d must confirm", c.Limits.VerifyVotes, c.Limits.VerifyMinConfirm)
	}

	devices := strings.Join(c.Resources.Devices.Serials, ", ")
	if devices == "" {
		devices = "none configured"
		if c.Resources.Devices.Discover {
			devices = "discovered from adb"
		}
	}

	return []configSection{
		{Title: "Agents", Note: "resolved per state at job start", Rows: []kvRow{
			{K: "backends", V: strings.Join(backends, ", "), Note: fmt.Sprintf("%d configured", len(backends))},
			{K: "review independence", V: independence, Note: "refuses to start otherwise"},
			{K: "reviewer writes", V: reviewerWrites, Note: ""},
			{K: "agent retries", V: strconv.Itoa(c.Limits.MaxAgentRetries), Note: ""},
		}},
		{Title: "Gates", Note: "which states park for a human", Rows: []kvRow{
			{K: "armed gates", V: strings.Join(armed, ", "), Note: "merge and release always"},
			{K: "design review blocks at", V: c.Policies.DesignReviewBlocksAt, Note: ""},
			{K: "code review blocks at", V: c.Policies.CodeReviewBlocksAt, Note: ""},
			{K: "final review blocks at", V: c.Policies.FinalReviewBlocksAt, Note: ""},
			{K: "finding verification", V: verify, Note: "gating findings only"},
			{K: "protected branches", V: strings.Join(c.Policies.ProtectedBranches, ", "), Note: "never a merge target"},
		}},
		{Title: "Budgets", Note: "enforced by the engine, not the agent", Rows: s.budgetRows()},
		{Title: "Machine", Note: "shared across every job", Rows: []kvRow{
			{K: "max parallel jobs", V: strconv.Itoa(c.Orchestrator.MaxParallelJobs), Note: ""},
			{K: "poll interval", V: c.Orchestrator.PollInterval.String(), Note: ""},
			{K: "gradle slots", V: strconv.Itoa(c.Resources.GradleSlots), Note: "concurrent"},
			{K: "devices", V: devices, Note: "leased per test run"},
			{K: "worktree cleanup", V: c.Git.CleanupWorktrees, Note: ""},
			{K: "serve address", V: c.Server.Listen, Note: "loopback only"},
		}},
	}
}

// --- /config/history -------------------------------------------------------

// configHistoryDirName must match the literal writeWithBackup uses in
// internal/config/live.go exactly — it is not exported (Owner B does not
// touch internal/config), so it is duplicated here rather than imported.
const configHistoryDirName = ".sdlc-config-history"

// configHistoryDir is the directory Live's write path backs a replaced config
// file up into, next to the file itself.
func configHistoryDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), configHistoryDirName)
}

type configHistoryPage struct {
	Dir   string
	Files []fileRow
	// Missing covers two cases the page does not need to tell apart: no
	// config file is loaded at all (Dir is ""), or one is loaded but nothing
	// has ever been saved through it, so the backup directory does not exist
	// yet. Both read the same to an operator: there is no history to show.
	Missing bool
}

func (s *Server) handleConfigHistory(w http.ResponseWriter, r *http.Request) {
	path := s.cfg.Get().Path
	body := configHistoryPage{}
	if path == "" {
		body.Missing = true
		s.render(w, r, "config_history.html", s.page(r, "Config history", "config", body))
		return
	}
	body.Dir = configHistoryDir(path)
	ents, err := os.ReadDir(body.Dir)
	if err != nil {
		// No saves yet is the overwhelmingly likely reason this directory
		// does not exist; a real read error (permissions) would fail Apply's
		// own writes too, and that failure is reported there, not guessed at
		// here.
		body.Missing = true
		s.render(w, r, "config_history.html", s.page(r, "Config history", "config", body))
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		body.Files = append(body.Files, fileRow{
			Name:    e.Name(),
			Href:    "/config/history/" + url.PathEscape(e.Name()),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	// Newest first: the filenames are a UTC timestamp prefix (writeWithBackup,
	// internal/config/live.go), so lexical order is chronological order, and
	// the version an operator opens this page to look at is almost always the
	// one just before whatever is on disk now.
	sort.SliceStable(body.Files, func(a, b int) bool {
		return body.Files[a].Name > body.Files[b].Name
	})
	s.render(w, r, "config_history.html", s.page(r, "Config history", "config", body))
}

type configHistoryEntryPage struct {
	Name string
	Path string
	Text string
	// LoadHref is built here rather than in the template, matching every
	// other href in this package (fileRow.Href, logPage.FollowURL): a Go
	// value never gets interpolated into the middle of a URL string inside a
	// template, it is escaped into a complete href first.
	LoadHref string
}

func (s *Server) handleConfigHistoryEntry(w http.ResponseWriter, r *http.Request) {
	cfgPath := s.cfg.Get().Path
	name := r.PathValue("name")
	if cfgPath == "" {
		s.fail(w, r, http.StatusNotFound, fmt.Sprintf("no history entry named %q", name))
		return
	}
	p, err := safeName(configHistoryDir(cfgPath), name)
	if err != nil {
		if errors.Is(err, errUnsafeName) {
			s.fail(w, r, http.StatusBadRequest, "that is not a name this server will serve")
			return
		}
		s.fail(w, r, http.StatusNotFound, fmt.Sprintf("no history entry named %q", name))
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		s.log.Printf("reading config history %s: %v", p, err)
		s.fail(w, r, http.StatusInternalServerError, "that version could not be read")
		return
	}
	body := configHistoryEntryPage{
		Name: name, Path: p, Text: string(raw),
		LoadHref: "/config?load=" + url.QueryEscape(name),
	}
	s.render(w, r, "config_history_entry.html", s.page(r, "Config history: "+name, "config", body))
}

// absOrAsIs makes a relative path readable without hiding it. The default data
// dir is "./data", which means nothing to a reader who does not know which
// directory `sdlc serve` was started in.
func absOrAsIs(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// redactPlaceholder is what replaces a value this page will not print. It is
// not the empty string: a reader has to be able to tell "there is a credential
// here and you are not being shown it" from "this is unset".
const redactPlaceholder = "[redacted]"

// secretish matches the names of config keys, command-line flags and
// environment variables that conventionally carry a credential.
var secretish = regexp.MustCompile(`(?i)(token|secret|passwd|password|credential|api[-_]?key|private[-_]?key|access[-_]?key|apikey|authorization)`)

// redactedConfigYAML re-marshals cfg to YAML with anything that looks like a
// credential replaced, and reports whether anything was.
//
// This page has to redact even though no config field is declared as a secret,
// because config.Load expands ${VAR} into the loaded struct (config.expandEnv).
// The file on disk holds "${GITHUB_TOKEN}"; the *config.Config this server is
// holding holds the token itself, and it reaches here through fields — a
// backend's extra_args, an agent's argv_template, a ship command — whose names
// say nothing about what they carry. So two rules run: the key or flag that
// names the value looks like a credential, or the value is one an environment
// variable with a credential-shaped name is currently holding.
//
// It is deliberately over-eager. A config value wrongly hidden costs a reader
// one look at the file on disk; a token rendered into a page costs a token.
func redactedConfigYAML(cfg *config.Config) (string, bool, error) {
	var root yaml.Node
	if err := root.Encode(cfg); err != nil {
		return "", false, err
	}
	env := secretEnvValues()
	redacted := redactNode(&root, "", env)
	out, err := yaml.Marshal(&root)
	if err != nil {
		return "", false, err
	}
	return string(out), redacted, nil
}

// The two floors below are what keeps the value rule from redacting the page.
//
// A credential-shaped environment value is looked for two ways. A long one is
// looked for *inside* a config scalar, because that is how it arrives: expanded
// into the middle of an argv element or a URL. A short one is only matched
// whole, because a four-character value appears as a substring of half a config
// file and substring-matching it would black out settings that have nothing to
// do with it — while an exact match on a short value costs at most one setting
// that happens to read the same, which is the trade this file already makes.
//
// Below minSecretValue nothing is collected at all: matching a config scalar of
// "1" against SOME_TOKEN=1 is noise, not redaction.
const (
	minSecretValue     = 4
	minSubstringSecret = 8
)

// secretEnvValues collects the values of environment variables whose names look
// like credentials.
func secretEnvValues() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || len(value) < minSecretValue || !secretish.MatchString(name) {
			continue
		}
		out = append(out, value)
	}
	return out
}

// holdsEnvSecret reports whether a config scalar carries one of those values —
// contained for the long ones, equal for the short ones.
func holdsEnvSecret(value string, env []string) bool {
	for _, v := range env {
		if len(v) >= minSubstringSecret {
			if strings.Contains(value, v) {
				return true
			}
			continue
		}
		if value == v {
			return true
		}
	}
	return false
}

// redactNode walks the encoded tree, replacing scalars under key, and reports
// whether it changed anything.
func redactNode(n *yaml.Node, key string, env []string) bool {
	changed := false
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			changed = redactNode(c, key, env) || changed
		}
	case yaml.MappingNode:
		// Content alternates key, value. The key's own scalar is never
		// redacted — hiding the name of the setting would hide that the
		// setting exists.
		for i := 0; i+1 < len(n.Content); i += 2 {
			changed = redactNode(n.Content[i+1], n.Content[i].Value, env) || changed
		}
	case yaml.SequenceNode:
		// A sequence inherits its parent's key, so `extra_args` under a
		// credential-shaped name redacts every element; and an argv list is
		// scanned for the flag/value pairing separately below.
		for _, c := range n.Content {
			changed = redactNode(c, key, env) || changed
		}
		changed = redactArgv(n.Content) || changed
	case yaml.ScalarNode:
		if n.Value == "" || n.Tag == "!!null" {
			return false
		}
		if secretish.MatchString(key) {
			setRedacted(n)
			return true
		}
		if holdsEnvSecret(n.Value, env) {
			setRedacted(n)
			return true
		}
	}
	return changed
}

// redactArgv handles the shape a credential actually arrives in on a command
// line: either ["--api-key", "sk-live-…"] or ["--api-key=sk-live-…"]. Neither
// element is under a key that names a secret, so the key rule above never sees
// them.
func redactArgv(items []*yaml.Node) bool {
	changed := false
	for i, it := range items {
		if it.Kind != yaml.ScalarNode || it.Value == redactPlaceholder {
			continue
		}
		flag, value, hasValue := strings.Cut(it.Value, "=")
		if hasValue && strings.HasPrefix(flag, "-") && value != "" && secretish.MatchString(flag) {
			it.Value = flag + "=" + redactPlaceholder
			it.Style = 0
			changed = true
			continue
		}
		if i == 0 {
			continue
		}
		prev := items[i-1]
		if prev.Kind == yaml.ScalarNode && strings.HasPrefix(prev.Value, "-") && secretish.MatchString(prev.Value) {
			setRedacted(it)
			changed = true
		}
	}
	return changed
}

func setRedacted(n *yaml.Node) {
	n.Value = redactPlaceholder
	n.Tag = "!!str"
	// Style is cleared so the replacement is emitted plainly whatever quoting
	// the original value needed.
	n.Style = 0
}
