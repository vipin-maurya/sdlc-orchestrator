package config

import (
	"path/filepath"
	"testing"
)

// sdlc.example.yaml is the file users copy. Strict decoding rejects unknown
// keys, so a key documented there but never added to the schema — or renamed
// in the schema and left behind there — breaks every config derived from it.
func TestExampleConfigDecodes(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "sdlc.example.yaml"))
	if err != nil {
		t.Fatalf("sdlc.example.yaml does not load: %v", err)
	}
	if !cfg.Orchestrator.StreamOutput {
		t.Error("stream_output did not decode from the example config")
	}
	if cfg.Orchestrator.HeartbeatInterval.D() == 0 {
		t.Error("heartbeat_interval did not decode from the example config")
	}
	if _, ok := cfg.Backends["claude"]; !ok {
		t.Error("claude backend missing from the example config")
	}
}
