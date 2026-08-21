// Package cli implements the sdlc subcommands (SPEC §11). Only `run` hosts
// the engine; every other command reads or writes the SQLite DB and exits,
// letting the running engine pick changes up on its next tick.
package cli

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/engine"
	"github.com/vipinm/sdlc-orchestrator/internal/gitx"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

const Version = "0.1.0"

const usage = `sdlc — autonomous SDLC orchestrator

Usage:
  sdlc [--config sdlc.yaml] <command> [args]

Commands:
  submit    --target <key> --title "..." (--body "..." | --file issue.md)
  run       [--once]                 start the engine (foreground)
  status    [JOB-ID]                 list jobs / show one job in detail
  review    [JOB-ID] [--diff] [--no-prompt]
                                     show what a job is waiting on you to
                                     decide; on a terminal, decide it
  approve   <JOB-ID> [--note "..."]  approve the pending merge/release gate
  reject    <JOB-ID> --reason "..." [--cancel]
  cancel    <JOB-ID>
  resume    <JOB-ID> [--to STATE] [--note "..."]
                                     clear an ESCALATED/TIMED_OUT/quota hold;
                                     --note instructs the next agent directly
  events    <JOB-ID>                 event log
  logs      <JOB-ID> [--last]        artifact & log paths; --last tails the most
                                     recent log, including a state still running
  validate  [--smoke]                config + environment doctor
  version
`

func Main(args []string) int {
	fs := flag.NewFlagSet("sdlc", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to sdlc.yaml (default: $SDLC_CONFIG, then ./sdlc.yaml)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, rest := rest[0], rest[1:]

	if cmd == "version" {
		fmt.Println("sdlc", Version)
		return 0
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	switch cmd {
	case "submit":
		return cmdSubmit(cfg, rest)
	case "run":
		return cmdRun(cfg, rest)
	case "status":
		return cmdStatus(cfg, rest)
	case "review":
		return cmdReview(cfg, rest)
	case "approve":
		return cmdDecision(cfg, rest, "approve")
	case "reject":
		return cmdDecision(cfg, rest, "reject")
	case "cancel":
		return cmdControl(cfg, rest, "cancel")
	case "resume":
		return cmdResume(cfg, rest)
	case "events":
		return cmdEvents(cfg, rest)
	case "logs":
		return cmdLogs(cfg, rest)
	case "validate":
		return cmdValidate(cfg, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

func openStore(cfg *config.Config) (*store.Store, error) {
	return store.Open(cfg.Database.Path, cfg.Database.BusyTimeout.D())
}

// parseArgs takes want as the exact number of positional arguments the command
// accepts; anything beyond it is an error rather than something quietly
// dropped, because a stray argument is a typo and dropping it is how a typo
// stays invisible for a whole run.
func parseArgs(fs *flag.FlagSet, args []string, want int, usage string) ([]string, error) {
	return parseArgsRange(fs, args, want, want, usage)
}

// parseArgsRange parses flags that may appear before, after, or between
// positional arguments. Go's flag package stops at the first non-flag token, so
// `sdlc resume JOB-1 --to BUILDING` would otherwise leave --to unparsed and
// silently ignored — which once resumed a job into the wrong state. Parsing
// resumes after each positional is consumed, so both orderings behave alike.
//
// min and max bound the positional count. They differ only for a command whose
// argument is optional — `sdlc review [JOB-ID]` takes zero or one — so every
// other command keeps the exact-count check it has always had.
func parseArgsRange(fs *flag.FlagSet, args []string, min, max int, usage string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	if len(pos) < min {
		return nil, fmt.Errorf("usage: %s", usage)
	}
	if len(pos) > max {
		return nil, fmt.Errorf("unexpected argument(s) %s; usage: %s", strings.Join(pos[max:], " "), usage)
	}
	return pos, nil
}

// argFail reports a usage error consistently.
func argFail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 2
}

func cmdSubmit(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	target := fs.String("target", "", "target key from sdlc.yaml targets")
	title := fs.String("title", "", "issue title")
	body := fs.String("body", "", "issue body text")
	file := fs.String("file", "", "read issue body from this file (first line becomes the title when --title is empty)")
	if _, err := parseArgs(fs, args, 0,
		`sdlc submit --target <key> --title "..." (--body "..." | --file issue.md)`); err != nil {
		return argFail(err)
	}

	if *target == "" {
		fmt.Fprintln(os.Stderr, "error: --target is required")
		return 2
	}
	t, err := cfg.Target(*target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	issueBody := *body
	issueTitle := *title
	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		issueBody = string(data)
		if issueTitle == "" {
			lines := strings.SplitN(strings.TrimSpace(issueBody), "\n", 2)
			issueTitle = strings.TrimSpace(strings.TrimPrefix(lines[0], "#"))
			if len(lines) > 1 {
				issueBody = strings.TrimSpace(lines[1])
			}
		}
	}
	if issueTitle == "" {
		fmt.Fprintln(os.Stderr, "error: --title (or --file with a heading) is required")
		return 2
	}
	if issueBody == "" {
		issueBody = issueTitle
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	id, err := st.NextJobID(cfg.Orchestrator.JobIDPrefix)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	job := &store.Job{
		ID:           id,
		Target:       *target,
		IssueTitle:   issueTitle,
		IssueBody:    issueBody,
		Branch:       t.BranchPrefix + id,
		WorktreePath: filepath.Join(t.WorktreesDir, id),
		State:        "CREATED",
	}
	if err := st.CreateJob(job); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("%s submitted (target %s, branch %s)\n", id, *target, job.Branch)
	fmt.Println("start the engine with: sdlc run")
	return 0
}

func cmdRun(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	once := fs.Bool("once", false, "drain runnable work, then exit")
	if _, err := parseArgs(fs, args, 0, "sdlc run [--once]"); err != nil {
		return argFail(err)
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	eng := engine.New(cfg, st, log.New(os.Stdout, "", log.LstdFlags))
	if err := eng.Run(ctx, *once); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func cmdStatus(cfg *config.Config, args []string) int {
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return statusOne(cfg, st, args[0])
	}
	jobs, err := st.ListJobs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if len(jobs) == 0 {
		fmt.Println("no jobs")
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tTARGET\tSTATE\tSINCE\tTITLE")
	for _, j := range jobs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", j.ID, j.Target, j.State,
			humanSince(j.StateEnteredAt), truncate(j.IssueTitle, 60))
	}
	w.Flush()
	printWaiting(jobs)
	return 0
}

// printWaiting names the jobs that have stopped for a human. The table above
// lists states, and a state name is not a request: an operator scanning it has
// to know which of a dozen states mean "you". Nothing is printed when nothing
// is waiting, so an idle run keeps the output it has always had.
func printWaiting(jobs []*store.Job) {
	var waiting []*store.Job
	for _, j := range jobs {
		if review.GateFor(j.State) != "" {
			waiting = append(waiting, j)
		}
	}
	if len(waiting) == 0 {
		return
	}
	fmt.Printf("\n%d job(s) waiting on you:\n", len(waiting))
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	for _, j := range waiting {
		reason := review.GateFor(j.State) + " gate"
		if review.GateFor(j.State) == review.GateHold {
			reason = truncate(j.HoldReason, 60)
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", j.ID, j.State, humanSince(j.StateEnteredAt), reason)
	}
	w.Flush()
	fmt.Println("run `sdlc review` to read and decide them.")
}

func statusOne(cfg *config.Config, st *store.Store, id string) int {
	j, err := st.GetJob(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("%s  [%s]\n", j.ID, j.State)
	fmt.Printf("  target:    %s\n  branch:    %s\n  worktree:  %s\n", j.Target, j.Branch, j.WorktreePath)
	fmt.Printf("  title:     %s\n", j.IssueTitle)
	fmt.Printf("  created:   %s   state entered: %s (%s ago)\n",
		j.CreatedAt.Local().Format(time.RFC3339), j.StateEnteredAt.Local().Format(time.RFC3339),
		humanSince(j.StateEnteredAt))
	if j.HoldReason != "" {
		fmt.Printf("  hold:      %s\n", j.HoldReason)
	}
	if !j.ResumeAfter.IsZero() {
		fmt.Printf("  resumes:   %s\n", j.ResumeAfter.Local().Format(time.RFC3339))
	}
	c := j.Counters
	fmt.Printf("  counters:  design_review=%d code_review=%d fixes=%d flake=%d release=%d agent_invocations=%d\n",
		c.DesignReviewRounds, c.CodeReviewRounds, c.FixAttempts, c.FlakeRetries, c.ReleaseRetries, c.AgentInvocations)
	fmt.Printf("  artifacts: %s\n", artifact.ArtifactsDir(cfg.Orchestrator.DataDir, j.ID))
	printLastProgress(st, j)
	switch j.State {
	case engine.SAwaitSpec:
		fmt.Println("\n  ACTION NEEDED: approve the spec and plan before any code is written")
	case engine.SAwaitCode:
		fmt.Println("\n  ACTION NEEDED: approve the implementation before it goes to build and test")
	case "AWAITING_MERGE_APPROVAL":
		fmt.Println("\n  ACTION NEEDED: review the change, then `sdlc approve " + j.ID + "` (or reject --reason)")
		printApprovalContext(cfg, j)
	case "AWAITING_RELEASE_APPROVAL":
		fmt.Println("\n  ACTION NEEDED: merged. Approve release with `sdlc approve " + j.ID + "` (or reject --reason)")
	case "ESCALATED", "TIMED_OUT":
		fmt.Println("\n  ACTION NEEDED: inspect artifacts/logs, then `sdlc resume " + j.ID + " [--to STATE]` or `sdlc cancel " + j.ID + "`")
	}
	// Every gate — including the two above that print nothing else — gets the
	// one command that shows what is actually being decided. The document is
	// named only once the engine has written it, so the path never dangles.
	if gate := review.GateFor(j.State); gate != "" {
		fmt.Printf("    read it:  sdlc review %s\n", j.ID)
		if p := review.DocPath(cfg.Orchestrator.DataDir, j.ID, gate); fileExists(p) {
			fmt.Printf("    document: %s\n", p)
		}
	}
	return 0
}

// printLastProgress surfaces the most recent progress event. A state that
// has been running for eight minutes should say what it is running; without
// it, "in flight" and "hung" are indistinguishable from the outside.
func printLastProgress(st *store.Store, j *store.Job) {
	evs, err := st.ListEvents(j.ID)
	if err != nil {
		return
	}
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if e.Kind != "progress" {
			continue
		}
		fmt.Printf("  running:   %s (%s ago)  %s\n", e.State, humanSince(e.CreatedAt), truncate(e.Detail, 90))
		return
	}
}

func printApprovalContext(cfg *config.Config, j *store.Job) {
	t, err := cfg.Target(j.Target)
	if err != nil {
		return
	}
	repo := gitx.Repo{Root: t.RepoPath}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if stat, err := repo.DiffStatSince(ctx, j.WorktreePath, j.Counters.BaseSHA); err == nil {
		fmt.Println("\n  diff stat vs base:")
		for _, ln := range strings.Split(strings.TrimRight(stat, "\n"), "\n") {
			fmt.Println("   ", ln)
		}
	}
	final := filepath.Join(artifact.ArtifactsDir(cfg.Orchestrator.DataDir, j.ID), "final_review.json")
	if r, err := artifact.LoadReview(final); err == nil {
		fmt.Printf("\n  final review: %s\n", r.Summary)
		for _, f := range r.Findings {
			fmt.Printf("    [%s] %s — %s\n", f.Severity, f.Description, f.Recommendation)
		}
	}
}

// recordDecision is the single place a human decision becomes an approval row.
// cmdDecision, cmdControl, cmdResume and the `sdlc review` prompt all go
// through it, so the engine sees one shape of row no matter which surface a
// person used; a second writer is how the four surfaces would drift apart.
func recordDecision(st *store.Store, a *store.Approval) error {
	return st.AddApproval(a)
}

func cmdDecision(cfg *config.Config, args []string, decision string) int {
	fs := flag.NewFlagSet(decision, flag.ContinueOnError)
	note := fs.String("note", "", "note recorded with the approval")
	reason := fs.String("reason", "", "reason (required for reject)")
	cancelFlag := fs.Bool("cancel", false, "on reject: cancel the job instead of sending it back to FIXING")
	pos, err := parseArgs(fs, args, 1,
		fmt.Sprintf("sdlc %s <JOB-ID> [--note \"...\"] [--reason \"...\"] [--cancel]", decision))
	if err != nil {
		return argFail(err)
	}
	id := pos[0]
	if decision == "reject" && *reason == "" {
		fmt.Fprintln(os.Stderr, "error: reject requires --reason")
		return 2
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	j, err := st.GetJob(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: job not found:", id)
		return 1
	}
	// review.GateFor is the one state->gate mapping; a second copy here is what
	// made `sdlc approve` blind to any gate added after it was written.
	// GateHold is not an approval gate: a held job is cleared with
	// resume/cancel, so approve/reject must still refuse it.
	gate := review.GateFor(j.State)
	if gate == "" || gate == review.GateHold {
		fmt.Fprintf(os.Stderr, "error: %s is in state %s — there is no pending approval gate\n", id, j.State)
		if gate == review.GateHold {
			fmt.Fprintf(os.Stderr, "  it is held; use `sdlc resume %s` or `sdlc cancel %s`\n", id, id)
		}
		return 1
	}
	r := *reason
	if r == "" {
		r = *note
	}
	if err := recordDecision(st, &store.Approval{JobID: id, Gate: gate, Decision: decision, Reason: r, Cancel: *cancelFlag}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf(decisionRecorded, id, decision, gate)
	return 0
}

func cmdControl(cfg *config.Config, args []string, gate string) int {
	fs := flag.NewFlagSet(gate, flag.ContinueOnError)
	pos, err := parseArgs(fs, args, 1, fmt.Sprintf("sdlc %s <JOB-ID>", gate))
	if err != nil {
		return argFail(err)
	}
	id := pos[0]
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	if _, err := st.GetJob(id); err != nil {
		fmt.Fprintln(os.Stderr, "error: job not found:", id)
		return 1
	}
	if err := recordDecision(st, &store.Approval{JobID: id, Gate: gate, Decision: gate}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("%s %s requested; the engine will act on its next tick\n", id, gate)
	return 0
}

func cmdResume(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	to := fs.String("to", "", "state to resume into (default: the state the job was in before the hold)")
	note := fs.String("note", "", "instruction handed to the agent in the resumed state (e.g. \"those fixtures are stale, update them\")")
	pos, err := parseArgs(fs, args, 1, `sdlc resume <JOB-ID> [--to STATE] [--note "..."]`)
	if err != nil {
		return argFail(err)
	}
	id := pos[0]
	if *to != "" && !engine.IsResumableState(*to) {
		fmt.Fprintf(os.Stderr, "error: --to %q is not a state a job can be resumed into\n  valid: %s\n",
			*to, strings.Join(engine.ResumableStates(), ", "))
		return 2
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	if _, err := st.GetJob(id); err != nil {
		fmt.Fprintln(os.Stderr, "error: job not found:", id)
		return 1
	}
	if err := recordDecision(st, &store.Approval{
		JobID: id, Gate: "resume", Decision: "resume", Reason: *to, Note: *note,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("%s resume requested; the engine will act on its next tick\n", id)
	if *note != "" {
		fmt.Println("  the note will be handed to the agent in the resumed state")
	}
	return 0
}

func cmdEvents(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	pos, err := parseArgs(fs, args, 1, "sdlc events <JOB-ID>")
	if err != nil {
		return argFail(err)
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	evs, err := st.ListEvents(pos[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tSTATE\tKIND\tAGENT\tEXIT\tDUR\tDETAIL")
	for _, e := range evs {
		exit := ""
		if e.ExitCode.Valid {
			exit = fmt.Sprint(e.ExitCode.Int64)
		}
		agent := e.Agent
		if agent != "" && e.Model != "" {
			agent += "(" + e.Model + ")"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			e.CreatedAt.Local().Format("15:04:05"), e.State, e.Kind, agent, exit,
			(time.Duration(e.DurationMS) * time.Millisecond).Round(time.Second), truncate(e.Detail, 80))
	}
	w.Flush()
	return 0
}

func cmdLogs(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	// Agent logs are written as the agent produces output, so the most recent
	// log belongs to the state currently running — this is the way to see what
	// an in-flight state is doing, not just what a finished one did.
	last := fs.Bool("last", false, "print the tail of the most recent log (a running state included)")
	pos, err := parseArgs(fs, args, 1, "sdlc logs <JOB-ID> [--last]")
	if err != nil {
		return argFail(err)
	}
	id := pos[0]
	logsDir := artifact.LogsDir(cfg.Orchestrator.DataDir, id)
	artDir := artifact.ArtifactsDir(cfg.Orchestrator.DataDir, id)
	fmt.Println("artifacts:", artDir)
	fmt.Println("logs:     ", logsDir)
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Println("  ", n)
	}
	if *last && len(names) > 0 {
		p := filepath.Join(logsDir, names[len(names)-1])
		data, err := os.ReadFile(p)
		if err == nil {
			lines := strings.Split(string(data), "\n")
			if len(lines) > 60 {
				lines = lines[len(lines)-60:]
			}
			fmt.Printf("\n--- tail of %s ---\n%s\n", names[len(names)-1], strings.Join(lines, "\n"))
		}
	}
	return 0
}

func humanSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// lookBinary resolves a binary path or PATH name.
func lookBinary(bin string) (string, error) {
	if filepath.IsAbs(bin) {
		if _, err := os.Stat(bin); err != nil {
			return "", err
		}
		return bin, nil
	}
	return exec.LookPath(bin)
}
