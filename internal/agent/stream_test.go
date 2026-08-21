package agent

import "testing"

func TestCondenseLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "tool use names what it acted on",
			in:   `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"app/src/Parser.kt"}}]}}`,
			want: "Edit app/src/Parser.kt",
		},
		{
			name: "bash shows the command",
			in:   `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls -la"}}]}}`,
			want: "Bash ls -la",
		},
		{
			name: "tool with no recognised input still names the tool",
			in:   `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{"todos":[]}}]}}`,
			want: "TodoWrite",
		},
		{
			name: "assistant prose is reduced to its first line",
			in:   `{"type":"assistant","message":{"content":[{"type":"text","text":"Reading the parser\nthen editing it"}]}}`,
			want: "Reading the parser",
		},
		{
			name: "reasoning is not an action",
			in:   `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"}]}}`,
			want: "",
		},
		{
			// The caller already records exit code and token counts from the
			// result envelope; echoing it as progress would be noise.
			name: "result envelope is dropped",
			in:   `{"type":"result","subtype":"success","usage":{"input_tokens":10}}`,
			want: "",
		},
		{
			name: "tool results are dropped",
			in:   `{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
			want: "",
		},
		{
			name: "system init is dropped",
			in:   `{"type":"system","subtype":"init","tools":["Read"]}`,
			want: "",
		},
		{
			name: "a backend that prints prose passes through",
			in:   "  compiling module app  ",
			want: "compiling module app",
		},
		{
			name: "malformed JSON is text, not an error",
			in:   `{"type":"assistant",`,
			want: `{"type":"assistant",`,
		},
		{
			name: "blank line yields nothing",
			in:   "   ",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CondenseLine(tc.in); got != tc.want {
				t.Errorf("CondenseLine(%s)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCondenseLineTruncates(t *testing.T) {
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	got := CondenseLine(string(long))
	if n := len([]rune(got)); n > maxProgressLine {
		t.Errorf("line not truncated: %d runes", n)
	}
	if !hasEllipsis(got) {
		t.Errorf("truncation not marked: %q", got)
	}
}

func hasEllipsis(s string) bool {
	r := []rune(s)
	return len(r) > 0 && r[len(r)-1] == '…'
}

// Several blocks in one message become one line, in order.
func TestCondenseLineJoinsBlocks(t *testing.T) {
	in := `{"type":"assistant","message":{"content":[
		{"type":"text","text":"now editing"},
		{"type":"tool_use","name":"Write","input":{"file_path":"a.kt"}}]}}`
	if got, want := CondenseLine(in), "now editing | Write a.kt"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
