// Package prompts embeds the default per-state prompt templates (SPEC §6.3).
// A path under states.<STATE>.prompt in sdlc.yaml overrides the embedded
// default; the template names here are the canonical ones.
package prompts

import "embed"

//go:embed *.md
var FS embed.FS
