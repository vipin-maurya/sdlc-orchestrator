// Package guard implements the mechanical policy checks from SPEC §13 that
// are enforced by the orchestrator, never by prompting: test-file diff
// policy, reviewer immutability, and budget checks.
package guard

import (
	"fmt"
	"regexp"
	"strings"
)

// Glob matches paths against a **-style glob:
//   **  any number of path segments (including none)
//   *   any run of characters within one segment
//   ?   one character within a segment
// Paths are compared slash-separated, case-insensitively (Windows target).
type Glob struct {
	src string
	re  *regexp.Regexp
}

func CompileGlob(pattern string) (Glob, error) {
	p := strings.ReplaceAll(pattern, "\\", "/")
	var sb strings.Builder
	sb.WriteString("(?i)^")
	i := 0
	for i < len(p) {
		c := p[i]
		switch c {
		case '*':
			if strings.HasPrefix(p[i:], "**/") {
				sb.WriteString(`(?:[^/]+/)*`)
				i += 3
				continue
			}
			if p[i:] == "**" {
				sb.WriteString(`.*`)
				i += 2
				continue
			}
			sb.WriteString(`[^/]*`)
			i++
		case '?':
			sb.WriteString(`[^/]`)
			i++
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return Glob{}, fmt.Errorf("glob %q: %w", pattern, err)
	}
	return Glob{src: pattern, re: re}, nil
}

func (g Glob) Match(path string) bool {
	return g.re.MatchString(strings.ReplaceAll(path, "\\", "/"))
}

type GlobSet []Glob

func CompileGlobs(patterns []string) (GlobSet, error) {
	out := make(GlobSet, 0, len(patterns))
	for _, p := range patterns {
		g, err := CompileGlob(p)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// Matching returns the subset of paths matching any glob in the set.
func (s GlobSet) Matching(paths []string) []string {
	var out []string
	for _, p := range paths {
		for _, g := range s {
			if g.Match(p) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// CheckFixDiff enforces the test-file policy (SPEC §13.1): when the active
// classification is code_bug and protection is on, the fix diff may not touch
// any file matching the test globs. Returns the violating files.
func CheckFixDiff(changed []string, classification string, protect bool, testGlobs GlobSet) []string {
	if !protect || classification != "code_bug" {
		return nil
	}
	return testGlobs.Matching(changed)
}
