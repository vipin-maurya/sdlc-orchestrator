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
  approve   <JOB-ID> [--note "..."]  approve the pending merge/release gate
  reject    <JOB-ID> --reason "..." [--cancel]
  cancel    <JOB-ID>
  resume    <JOB-ID> [--to STATE]    clear an ESCALATED/TIMED_OUT/quota hold
  events    <JOB-ID>                 event log
  logs      <JOB-ID> [--last]        artifact & log paths (tail last log)
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

func cmdSubmit(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	target := fs.String("target", "", "target key from sdlc.yaml targets")
	title := fs.String("title", "", "issue title")
	body := fs.String("body", "", "issue body text")
	file := fs.String("file", "", "read issue body from this file (first line becomes the title when --title is empty)")
	fs.Parse(args)

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
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	once := fs.Bool("once", false, "drain runnable work, then exit")
	fs.Parse(args)
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
	return 0
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
	fmt.Printf("  created:   %s   state entered: %s\n", j.CreatedAt.Local().Format(time.RFC3339), j.StateEnteredAt.Local().Format(time.RFC3339))
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
	switch j.State {
	case "AWAITING_MERGE_APPROVAL":
		fmt.Println("\n  ACTION NEEDED: review the change, then `sdlc approve " + j.ID + "` (or reject --reason)")
		printApprovalContext(cfg, j)
	case "AWAITING_RELEASE_APPROVAL":
		fmt.Println("\n  ACTION NEEDED: merged. Approve release with `sdlc approve " + j.ID + "` (or reject --reason)")
	case "ESCALATED", "TIMED_OUT":
		fmt.Println("\n  ACTION NEEDED: inspect artifacts/logs, then `sdlc resume " + j.ID + " [--to STATE]` or `sdlc cancel " + j.ID + "`")
	}
	return 0
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

func gateFor(state string) string {
	switch state {
	case "AWAITING_MERGE_APPROVAL":
		return "merge"
	case "AWAITING_RELEASE_APPROVAL":
		return "release"
	}
	return ""
}

func cmdDecision(cfg *config.Config, args []string, decision string) int {
	fs := flag.NewFlagSet(decision, flag.ExitOnError)
	note := fs.String("note", "", "note recorded with the approval")
	reason := fs.String("reason", "", "reason (required for reject)")
	cancelFlag := fs.Bool("cancel", false, "on reject: cancel the job instead of sending it back to FIXING")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "usage: sdlc %s <JOB-ID>\n", decision)
		return 2
	}
	id := fs.Arg(0)
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
	gate := gateFor(j.State)
	if gate == "" {
		fmt.Fprintf(os.Stderr, "error: %s is in state %s — there is no pending approval gate\n", id, j.State)
		return 1
	}
	r := *reason
	if r == "" {
		r = *note
	}
	if err := st.AddApproval(&store.Approval{JobID: id, Gate: gate, Decision: decision, Reason: r, Cancel: *cancelFlag}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("%s %s recorded for %s gate; the engine will act on its next tick\n", id, decision, gate)
	return 0
}

func cmdControl(cfg *config.Config, args []string, gate string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: sdlc %s <JOB-ID>\n", gate)
		return 2
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	if _, err := st.GetJob(args[0]); err != nil {
		fmt.Fprintln(os.Stderr, "error: job not found:", args[0])
		return 1
	}
	if err := st.AddApproval(&store.Approval{JobID: args[0], Gate: gate, Decision: gate}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("%s %s requested; the engine will act on its next tick\n", args[0], gate)
	return 0
}

func cmdResume(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	to := fs.String("to", "", "state to resume into (default: the state the job was in before the hold)")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: sdlc resume <JOB-ID> [--to STATE]")
		return 2
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	if _, err := st.GetJob(fs.Arg(0)); err != nil {
		fmt.Fprintln(os.Stderr, "error: job not found:", fs.Arg(0))
		return 1
	}
	if err := st.AddApproval(&store.Approval{JobID: fs.Arg(0), Gate: "resume", Decision: "resume", Reason: *to}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("%s resume requested; the engine will act on its next tick\n", fs.Arg(0))
	return 0
}

func cmdEvents(cfg *config.Config, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: sdlc events <JOB-ID>")
		return 2
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	evs, err := st.ListEvents(args[0])
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
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	last := fs.Bool("last", false, "print the tail of the most recent log")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: sdlc logs <JOB-ID> [--last]")
		return 2
	}
	id := fs.Arg(0)
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
