package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/execx"
	"github.com/vipinm/sdlc-orchestrator/internal/gitx"
)

// cmdValidate is the config + environment doctor (SPEC §11). Hard failures
// exit non-zero; warnings do not. --smoke additionally runs one tiny headless
// prompt through every backend used by a state (costs quota; off by default).
func cmdValidate(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	smoke := fs.Bool("smoke", false, "run a real one-line prompt through each configured backend (costs quota)")
	fs.Parse(args)

	fails, warns := 0, 0
	fail := func(format string, a ...any) { fails++; fmt.Printf("  FAIL  %s\n", fmt.Sprintf(format, a...)) }
	warn := func(format string, a ...any) { warns++; fmt.Printf("  warn  %s\n", fmt.Sprintf(format, a...)) }
	ok := func(format string, a ...any) { fmt.Printf("  ok    %s\n", fmt.Sprintf(format, a...)) }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Println("config:")
	ok("loaded %s (validated: schema, unknown keys, independence constraints)", cfg.Path)

	fmt.Println("data:")
	if err := os.MkdirAll(cfg.Orchestrator.DataDir, 0o755); err != nil {
		fail("data_dir %s: %v", cfg.Orchestrator.DataDir, err)
	} else {
		ok("data_dir %s writable", cfg.Orchestrator.DataDir)
	}
	if st, err := openStore(cfg); err != nil {
		fail("database %s: %v", cfg.Database.Path, err)
	} else {
		st.Close()
		ok("database %s opens (WAL)", cfg.Database.Path)
	}

	fmt.Println("backends:")
	usedBackends := map[string]config.Backend{}
	for _, sn := range config.AgentStates {
		if _, ag, b, _, err := cfg.AgentFor(sn); err == nil {
			usedBackends[ag.Backend] = b
		}
	}
	for name, b := range usedBackends {
		path, err := lookBinary(b.Binary)
		if err != nil {
			fail("backends.%s binary %q not found: %v", name, b.Binary, err)
			continue
		}
		_, out, _ := execx.RunCapture(ctx, execx.Cmd{Argv: []string{path, "--version"}, Timeout: 30 * time.Second}, 4096)
		ver := strings.TrimSpace(out)
		ok("backends.%s: %s (version: %s)", name, path, firstLine(ver))
		if b.ExpectedVersion != "" && !strings.Contains(ver, b.ExpectedVersion) {
			warn("backends.%s version %q does not match expected_version %q", name, firstLine(ver), b.ExpectedVersion)
		}
		if b.Kind == "agy" {
			validateAgySettings(name, b, ok, warn)
		}
	}

	fmt.Println("targets:")
	for key, t := range cfg.Targets {
		repo := gitx.Repo{Root: t.RepoPath}
		if err := repo.GitOK(ctx); err != nil {
			fail("targets.%s: %s is not a git repository", key, t.RepoPath)
			continue
		}
		ok("targets.%s: git repo at %s", key, t.RepoPath)
		if dirty, err := repo.IsDirty(ctx, t.RepoPath); err == nil && dirty {
			warn("targets.%s: main checkout is dirty — MERGING will refuse until it is clean", key)
		}
		if cur, err := repo.CurrentBranch(ctx, t.RepoPath); err == nil && cur != t.DefaultBranch {
			warn("targets.%s: main checkout is on %q, not default_branch %q (MERGING will switch it)", key, cur, t.DefaultBranch)
		}
		if len(t.Ship.Command) > 0 {
			if _, err := lookBinary(t.Ship.Command[0]); err != nil {
				warn("targets.%s: ship command binary %q not found on PATH", key, t.Ship.Command[0])
			} else {
				ok("targets.%s: ship binary %q found", key, t.Ship.Command[0])
			}
		}
		if t.UITest.Enabled {
			if _, err := lookBinary(cfg.Resources.Devices.AdbBinary); err != nil {
				fail("targets.%s has ui_test.enabled but adb (%q) not found", key, cfg.Resources.Devices.AdbBinary)
			} else {
				_, out, _ := execx.RunCapture(ctx, execx.Cmd{Argv: []string{cfg.Resources.Devices.AdbBinary, "devices"}, Timeout: 30 * time.Second}, 1<<14)
				n := 0
				for _, ln := range strings.Split(out, "\n") {
					f := strings.Fields(strings.TrimSpace(ln))
					if len(f) >= 2 && f[1] == "device" {
						n++
					}
				}
				if n == 0 && !cfg.Resources.Devices.BootEmulator.Enabled {
					warn("targets.%s: no adb devices connected and boot_emulator disabled (on_no_device=%s applies)", key, t.UITest.OnNoDevice)
				} else {
					ok("targets.%s: adb reachable, %d device(s) connected", key, n)
				}
			}
		}
	}

	if *smoke {
		fmt.Println("smoke (headless round-trip per backend):")
		for name, b := range usedBackends {
			if err := smokeBackend(ctx, b); err != nil {
				fail("backends.%s smoke: %v", name, err)
			} else {
				ok("backends.%s answered a headless prompt through a pipe", name)
			}
		}
	} else {
		fmt.Println("smoke: skipped (run `sdlc validate --smoke` to test real backend round-trips; costs quota)")
	}

	fmt.Printf("\n%d failure(s), %d warning(s)\n", fails, warns)
	if fails > 0 {
		return 1
	}
	return 0
}

// validateAgySettings checks the agy settings file and permission grants
// (SPEC §6.2): shell commands are soft-denied headlessly unless pre-granted.
func validateAgySettings(name string, b config.Backend, ok, warn func(string, ...any)) {
	if b.SettingsFile == "" {
		return
	}
	data, err := os.ReadFile(b.SettingsFile)
	if err != nil {
		warn("backends.%s: settings file %s unreadable (%v) — headless runs may soft-deny shell commands", name, b.SettingsFile, err)
		return
	}
	ok("backends.%s: settings file %s present", name, b.SettingsFile)
	if len(b.EnsurePermissions) == 0 {
		return
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		warn("backends.%s: settings file is not valid JSON: %v", name, err)
		return
	}
	var allow []any
	if p, ok2 := m["permissions"].(map[string]any); ok2 {
		allow, _ = p["allow"].([]any)
	}
	have := map[string]bool{}
	for _, v := range allow {
		if s, ok2 := v.(string); ok2 {
			have[s] = true
		}
	}
	var missing []string
	for _, want := range b.EnsurePermissions {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		warn("backends.%s: permissions.allow is missing %v — add them to %s or headless runs will soft-deny", name, missing, b.SettingsFile)
	} else {
		ok("backends.%s: all ensure_permissions grants present", name)
	}
}

// smokeBackend sends a trivial prompt and checks stdout is non-empty —
// specifically to catch the agy empty-stdout-through-a-pipe failure mode.
func smokeBackend(ctx context.Context, b config.Backend) error {
	var argv []string
	switch b.Kind {
	case "claude":
		argv = []string{b.Binary, "-p", "Reply with exactly: OK", "--output-format", "json"}
	case "agy":
		argv = []string{b.Binary, "-p", "Reply with exactly: OK", "--output-format", "json"}
	default:
		return nil
	}
	res, out, err := execx.RunCapture(ctx, execx.Cmd{Argv: argv, Timeout: 3 * time.Minute}, 1<<20)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("exit %d with EMPTY stdout through a pipe — this backend cannot be used headlessly as configured", res.ExitCode)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(out))
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
