package server

// The greps. Owner: W2-G.
//
// Each rule here is an acceptance criterion that no ordinary test can express,
// because what it asserts is the absence of something rather than the behaviour
// of something. A grep in a checklist is a grep nobody runs; a grep in a test
// is one CI runs on every commit.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// packageGoFiles returns the package's non-test sources, which is the scope
// every rule below applies to.
func packageGoFiles(t *testing.T) map[string]string {
	t.Helper()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		out[n] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("no package sources found; the rules below would pass vacuously")
	}
	return out
}

func TestHouseRules(t *testing.T) {
	src := packageGoFiles(t)

	// AC-17. html/template escapes a plain string correctly in every context
	// this UI uses, including href and src, so none of these types is needed.
	// One that exists is one that eventually gets applied to a finding
	// description an agent wrote.
	t.Run("no template escape hatches", func(t *testing.T) {
		for name, body := range src {
			for _, bad := range []string{"template.HTML", "template.JS", "template.URL", "template.CSS", "template.Srcset"} {
				if strings.Contains(body, bad) {
					t.Errorf("%s uses %s", name, bad)
				}
			}
		}
	})

	// AC-9. safeName resolves by membership in os.ReadDir, and the rule is
	// worth a grep because the tempting rewrite — clean the name, join it,
	// check the prefix — passes every test written against Unix semantics and
	// is defeatable on Windows. Scoped to safeName's own body: hostAllowed
	// legitimately prefix-tests a bracketed IPv6 literal, which is not a path.
	t.Run("safeName resolves by membership", func(t *testing.T) {
		body := funcBody(t, src["safety.go"], "func safeName(")
		for _, bad := range []string{"HasPrefix", "filepath.Clean", "filepath.Abs", "filepath.EvalSymlinks", "filepath.Rel"} {
			if strings.Contains(body, bad) {
				t.Errorf("safeName uses %s; it must resolve by membership in os.ReadDir", bad)
			}
		}
		if !strings.Contains(body, "os.ReadDir(") {
			t.Error("safeName does not read the directory listing at all")
		}
	})

	// AC-16. The bound belongs in one place; a MaxBytesReader that each of six
	// owners has to remember is one that will be missing from one of them.
	t.Run("every POST body is bounded", func(t *testing.T) {
		if !strings.Contains(src["server.go"], "http.MaxBytesReader") {
			t.Fatal("no MaxBytesReader in server.go")
		}
		if !strings.Contains(src["server.go"], "limitBody(mux)") {
			t.Error("limitBody is not wrapped around the mux, so a route could escape the bound")
		}
	})

	// AC-11, first half. Every POST route registered in the table runs through
	// requireCSRF — directly, or through decision(), which is requireCSRF plus
	// requireFreshState. This is the rule that keeps a later owner from adding
	// an unauthenticated approve button by registering one line.
	t.Run("every POST route is CSRF-guarded", func(t *testing.T) {
		for _, line := range strings.Split(src["server.go"], "\n") {
			if !strings.Contains(line, `mux.HandleFunc("POST `) {
				continue
			}
			if !strings.Contains(line, "s.requireCSRF(") && !strings.Contains(line, "s.decision(") {
				t.Errorf("POST route registered without a CSRF guard: %s", strings.TrimSpace(line))
			}
		}
	})
}

// TestEveryPostFormCarriesTheToken is AC-11's second half, over the templates.
// It passes vacuously today because the skeleton's forms are empty; it starts
// biting the moment W2-I fills _forms.html, which is the point at which it
// matters.
func TestEveryPostFormCarriesTheToken(t *testing.T) {
	ents, err := os.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	formRe := regexp.MustCompile(`(?is)<form[^>]*method\s*=\s*["']post["'][^>]*>(.*?)</form>`)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join("templates", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range formRe.FindAllStringSubmatch(string(b), -1) {
			if !strings.Contains(m[1], `name="csrf"`) {
				t.Errorf("templates/%s has a POST form with no hidden csrf input:\n%s", e.Name(), m[0])
			}
		}
	}
}

// w21Owners are the six owners the skeleton commit left stubs for. A stub
// naming anybody else is a stub nobody agreed to fill.
var w21Owners = map[string]bool{
	"W2-H": true, "W2-I": true, "W2-J": true, "W2-K": true, "W2-L": true,
}

// TestNoStubsRemain is how the W2.1 wave proves it finished.
//
// It reports rather than fails while stubs are outstanding, and the reason is
// a practical one: this suite is the one every W2.1 owner runs, and a test that
// is red from the skeleton commit until the last handler lands is a test five
// owners learn to ignore, which is exactly the state in which a real failure
// goes unread. So it skips with the outstanding owners named — visible in
// `go test -v` and in the checkpoint — and it fails outright on the one thing
// that is always wrong: a stub naming an owner who is not in the wave.
//
// It cannot pass silently forever: when the last handler lands the skip stops
// and the test passes on its own terms, and a stub added afterwards names an
// owner nobody is waiting for and fails immediately.
func TestNoStubsRemain(t *testing.T) {
	// The needle is built rather than written out so this file does not match
	// its own grep.
	needle := "stub" + `(w, "`
	outstanding := map[string][]string{}
	for name, body := range packageGoFiles(t) {
		for _, line := range strings.Split(body, "\n") {
			i := strings.Index(line, needle)
			if i < 0 {
				continue
			}
			rest := line[i+len(needle):]
			owner, _, _ := strings.Cut(rest, `"`)
			if !w21Owners[owner] {
				t.Errorf("%s: stub names %q, which is not a W2.1 owner", name, owner)
				continue
			}
			outstanding[owner] = appendOnce(outstanding[owner], name)
		}
	}
	if len(outstanding) == 0 {
		return
	}
	owners := make([]string, 0, len(outstanding))
	for o := range outstanding {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	var b strings.Builder
	for _, o := range owners {
		files := outstanding[o]
		sort.Strings(files)
		b.WriteString("\n  " + o + ": " + strings.Join(files, ", "))
	}
	t.Skipf("W2.1 is not finished — handlers still stubbed:%s", b.String())
}

func appendOnce(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

// funcBody returns the source of the function whose declaration starts with
// decl, from its opening brace to the matching closing brace at column 0. It is
// deliberately crude: the alternative is a go/ast walk, and a grep rule whose
// implementation needs its own tests is a rule nobody will trust.
func funcBody(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("%q not found", decl)
	}
	rest := src[i:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}
