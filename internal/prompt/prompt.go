// Package prompt renders per-state prompt templates (SPEC §6.3). Embedded
// defaults live in the prompts package; states.<STATE>.prompt overrides with
// a file path. Every rendered prompt is hashed (sha256) for the event log.
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/prompts"
)

// Ctx is the template context. Fields are "" / zero when not applicable to
// the state being rendered; templates guard with {{if}}.
type Ctx struct {
	JobID      string
	Branch     string
	Target     string
	IssueTitle string
	IssueBody  string

	// ScopedProblem is problem.json's content, inlined so the planner is bound
	// by it without having to be trusted to open the staged file. "" when
	// policies.scoping is off.
	ScopedProblem string

	Round     int    // current round/attempt within the loop (1-based)
	MaxRounds int    // the configured cap for that loop
	BaseSHA   string // commit before implementation started

	PrevFindings   string // JSON array of findings from the rejected round
	FailureExcerpt string
	FailedPhase    string // build | unit | ui
	FailingTargets string
	Classification string
	FixHint        string
	RejectReason   string

	// Verify pass only: which finding this verifier is testing and where its
	// verdict must be written. Round/MaxRounds carry the vote index and the
	// vote count, so each verifier knows it is one of several.
	FindingID  string
	OutputPath string
}

var defaults = map[string]string{
	config.StScoping:      "scoping.md",
	config.StPlanning:     "planning.md",
	config.StDesignReview: "design_review.md",
	config.StImplementing: "implementing.md",
	config.StCodeReview:   "code_review.md",
	config.StAnalyzing:    "analyzing.md",
	config.StFixing:       "fixing.md",
	config.StFinalReview:  "final_review.md",
	config.StVerifying:    "verify_finding.md",
}

// Render produces the prompt for state. overridePath ("" = embedded default)
// is resolved relative to configDir when not absolute.
func Render(state, overridePath, configDir string, ctx Ctx) (text string, hash string, err error) {
	var raw []byte
	if overridePath != "" {
		p := overridePath
		if !filepath.IsAbs(p) {
			p = filepath.Join(configDir, p)
		}
		raw, err = os.ReadFile(p)
		if err != nil {
			return "", "", fmt.Errorf("states.%s.prompt: %w", state, err)
		}
	} else {
		name, ok := defaults[state]
		if !ok {
			return "", "", fmt.Errorf("no default prompt for state %s", state)
		}
		raw, err = prompts.FS.ReadFile(name)
		if err != nil {
			return "", "", err
		}
	}
	tmpl, err := template.New(state).Parse(string(raw))
	if err != nil {
		return "", "", fmt.Errorf("prompt template for %s: %w", state, err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, ctx); err != nil {
		return "", "", fmt.Errorf("render prompt for %s: %w", state, err)
	}
	text = sb.String()
	sum := sha256.Sum256([]byte(text))
	return text, hex.EncodeToString(sum[:]), nil
}
