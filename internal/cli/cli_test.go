package cli

import (
	"flag"
	"strings"
	"testing"
)

// Go's flag package stops parsing at the first non-flag token. Every command
// here takes its job id positionally, so `sdlc resume JOB-1 --to BUILDING`
// left --to unparsed and silently ignored: the engine fell back to the
// previous state and resumed the job straight back into the state that had
// just escalated. Both orderings must produce the same result, and anything
// left over must be an error rather than something quietly dropped.
func TestParseArgsAcceptsFlagsInEitherPosition(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"flags after the positional", []string{"JOB-1", "--to", "BUILDING", "--note", "do it"}},
		{"flags before the positional", []string{"--to", "BUILDING", "--note", "do it", "JOB-1"}},
		{"flags on both sides", []string{"--to", "BUILDING", "JOB-1", "--note", "do it"}},
		{"single-dash form", []string{"JOB-1", "-to", "BUILDING", "-note", "do it"}},
		{"equals form", []string{"JOB-1", "--to=BUILDING", "--note=do it"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("resume", flag.ContinueOnError)
			fs.SetOutput(io_Discard{})
			to := fs.String("to", "", "")
			note := fs.String("note", "", "")
			pos, err := parseArgs(fs, tc.args, 1, "usage")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(pos) != 1 || pos[0] != "JOB-1" {
				t.Errorf("positional = %v, want [JOB-1]", pos)
			}
			if *to != "BUILDING" {
				t.Errorf("--to = %q, want BUILDING", *to)
			}
			if *note != "do it" {
				t.Errorf("--note = %q, want %q", *note, "do it")
			}
		})
	}
}

// A stray argument is a typo, not a thing to ignore. Ignoring it is how the
// original bug stayed invisible for a whole run.
func TestParseArgsRejectsExtraPositionals(t *testing.T) {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(io_Discard{})
	fs.String("to", "", "")
	_, err := parseArgs(fs, []string{"JOB-1", "JOB-2", "--to", "FIXING"}, 1, "sdlc resume <JOB-ID>")
	if err == nil {
		t.Fatal("extra positional accepted")
	}
	if !strings.Contains(err.Error(), "JOB-2") {
		t.Errorf("error does not name the stray argument: %v", err)
	}
}

func TestParseArgsRequiresThePositional(t *testing.T) {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(io_Discard{})
	fs.String("to", "", "")
	if _, err := parseArgs(fs, []string{"--to", "FIXING"}, 1, "sdlc resume <JOB-ID>"); err == nil {
		t.Fatal("missing positional accepted")
	}
}

// An unknown flag must fail rather than being swallowed as a positional.
func TestParseArgsRejectsUnknownFlags(t *testing.T) {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(io_Discard{})
	fs.String("to", "", "")
	if _, err := parseArgs(fs, []string{"JOB-1", "--nope", "x"}, 1, "usage"); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

type io_Discard struct{}

func (io_Discard) Write(p []byte) (int, error) { return len(p), nil }

// `sdlc review` is the first command whose positional argument is optional:
// with an id it decides one job, without one it walks everything waiting.
// parseArgsRange is what parseArgs became so that could work, so the bound it
// applies at each end is what needs holding — the four tests above prove the
// exact-count case still behaves, and this one proves the range case does.
func TestParseArgsRangeAcceptsZeroOrOne(t *testing.T) {
	const usage = "sdlc review [JOB-ID] [--diff] [--no-prompt]"
	cases := []struct {
		name    string
		args    []string
		wantPos []string
		wantErr string
	}{
		{"no positional at all", []string{}, nil, ""},
		{"flags only", []string{"--diff"}, nil, ""},
		{"one positional", []string{"JOB-1"}, []string{"JOB-1"}, ""},
		{"positional then flag", []string{"JOB-1", "--diff"}, []string{"JOB-1"}, ""},
		{"flag then positional", []string{"--diff", "JOB-1"}, []string{"JOB-1"}, ""},
		{"two positionals", []string{"JOB-1", "JOB-2"}, nil, "JOB-2"},
		{"unknown flag", []string{"JOB-1", "--nope"}, nil, "nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("review", flag.ContinueOnError)
			fs.SetOutput(io_Discard{})
			diff := fs.Bool("diff", false, "")
			pos, err := parseArgsRange(fs, tc.args, 0, 1, usage)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseArgsRange(%v) = %v, want an error naming %q", tc.args, pos, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %v does not name %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgsRange(%v): %v", tc.args, err)
			}
			if strings.Join(pos, ",") != strings.Join(tc.wantPos, ",") {
				t.Errorf("positional = %v, want %v", pos, tc.wantPos)
			}
			if wantDiff := contains(tc.args, "--diff"); *diff != wantDiff {
				t.Errorf("--diff = %v, want %v", *diff, wantDiff)
			}
		})
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
