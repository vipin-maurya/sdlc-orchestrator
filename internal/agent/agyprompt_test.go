package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A prompt that fits still travels inline. This is the path that always worked
// and the one most invocations take; the fix must not disturb it.
func TestAgyShortPromptStaysInline(t *testing.T) {
	s := Spec{Prompt: "write the thing", Cwd: t.TempDir()}
	got, err := agyPromptArg(s)
	if err != nil {
		t.Fatal(err)
	}
	if got != s.Prompt {
		t.Errorf("-p value = %q, want the prompt verbatim", got)
	}
	if _, err := os.Stat(filepath.Join(s.Cwd, ".sdlc", "prompt.md")); err == nil {
		t.Error("a short prompt should not write a prompt file")
	}
}

// R9: the prompt that killed JOB-2 was 38,867 bytes. Windows refuses to launch
// a process whose command line exceeds ~32,767 characters, so the -p value has
// to be small whatever the prompt is.
func TestAgyLongPromptGoesToAFileAndTheArgStaysSmall(t *testing.T) {
	body := strings.Repeat("implement the plan exactly as written. ", 1000) // ~39KB
	if len(body) <= maxArgPrompt {
		t.Fatalf("test prompt is only %d bytes; it must exceed maxArgPrompt (%d)", len(body), maxArgPrompt)
	}
	cwd := t.TempDir()
	got, err := agyPromptArg(Spec{Prompt: body, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}

	// The number that matters. 32767 is the CreateProcessW limit; the whole
	// command line has to fit, not just this argument, so the pointer is held
	// far below it rather than merely under it.
	if len(got) > 1000 {
		t.Errorf("-p value is %d bytes; it must stay far below the 32767 command-line limit", len(got))
	}
	if strings.Contains(got, body[:200]) {
		t.Error("-p value still carries the prompt text")
	}

	path := filepath.Join(cwd, ".sdlc", "prompt.md")
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("prompt file not written: %v", err)
	}
	if string(written) != body {
		t.Errorf("prompt file holds %d bytes, want the full %d", len(written), len(body))
	}
	// The pointer has to name the file the agent must open, or the agent is
	// told to read something it cannot find.
	if !strings.Contains(got, ".sdlc/prompt.md") {
		t.Errorf("-p value does not name the prompt file: %q", got)
	}
}

// The bug itself: -p must never be emitted without a value, because agy then
// takes the next argument as the prompt. Whatever else changes, the value has
// to be non-empty for every prompt size.
func TestAgyPromptArgIsNeverEmpty(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt string
	}{
		{"short", "do the thing"},
		{"exactly at the limit", strings.Repeat("x", maxArgPrompt)},
		{"one over the limit", strings.Repeat("x", maxArgPrompt+1)},
		{"very long", strings.Repeat("x", 200_000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := agyPromptArg(Spec{Prompt: tc.prompt, Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(got) == "" {
				t.Error("empty -p value: agy would read the next argument as the prompt")
			}
			// An argument that starts with a dash is the same failure wearing a
			// different hat — agy would treat it as a flag.
			if strings.HasPrefix(got, "-") {
				t.Errorf("-p value looks like a flag: %q", got)
			}
		})
	}
}

// A long prompt with no working directory (ad-hoc runs, tests) must still get a
// readable file and a pointer that resolves without one.
func TestAgyLongPromptWithoutCwdUsesAnAbsolutePointer(t *testing.T) {
	body := strings.Repeat("y", maxArgPrompt+1)
	got, err := agyPromptArg(Spec{Prompt: body})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.TempDir(), ".sdlc", "prompt.md")
	t.Cleanup(func() { os.Remove(path) })
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("prompt file not written without a Cwd: %v", err)
	}
	if !strings.Contains(got, filepath.ToSlash(os.TempDir())) {
		t.Errorf("pointer is not absolute when there is no working directory: %q", got)
	}
}
