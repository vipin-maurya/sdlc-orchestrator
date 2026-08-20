// Package resource implements the ResourceManager (SPEC §3.4): a bounded
// gradle-slot semaphore shared by all jobs, and a device pool for UI tests
// (explicit serials + adb discovery + optional emulator boot).
package resource

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/execx"
)

type Manager struct {
	cfg config.Resources

	gradle chan struct{}

	mu      sync.Mutex
	inUse   map[string]bool // device serial -> leased
	booted  map[string]bool // serials of emulators this process booted
	bootCmd *exec.Cmd
}

func NewManager(cfg config.Resources) *Manager {
	m := &Manager{
		cfg:    cfg,
		gradle: make(chan struct{}, cfg.GradleSlots),
		inUse:  map[string]bool{},
		booted: map[string]bool{},
	}
	return m
}

// AcquireGradle blocks until a gradle slot is free (or ctx is done).
func (m *Manager) AcquireGradle(ctx context.Context) (release func(), err error) {
	select {
	case m.gradle <- struct{}{}:
		return func() { <-m.gradle }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// adbDevices lists serials in "device" state.
func (m *Manager) adbDevices(ctx context.Context) ([]string, error) {
	res, out, err := execx.RunCapture(ctx, execx.Cmd{
		Argv:    []string{m.cfg.Devices.AdbBinary, "devices"},
		Timeout: 30 * time.Second,
	}, 1<<16)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("adb devices exit %d: %s", res.ExitCode, out)
	}
	var serials []string
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if ln == "" || strings.HasPrefix(ln, "List of devices") || strings.HasPrefix(ln, "*") {
			continue
		}
		parts := strings.Fields(ln)
		if len(parts) >= 2 && parts[1] == "device" {
			serials = append(serials, parts[0])
		}
	}
	return serials, nil
}

// AcquireDevice leases a device serial for a UI-test run, obeying
// resources.devices.* (SPEC §3.4). Returns ("", nil, nil) if no device could
// be obtained within acquire_timeout — the caller applies on_no_device.
func (m *Manager) AcquireDevice(ctx context.Context) (serial string, release func(), err error) {
	deadline := time.Now().Add(m.cfg.Devices.AcquireTimeout.D())
	bootTried := false
	for {
		if s := m.tryLease(ctx); s != "" {
			return s, func() { m.release(s) }, nil
		}
		if m.cfg.Devices.BootEmulator.Enabled && !bootTried {
			bootTried = true
			if err := m.bootEmulator(ctx); err != nil {
				return "", nil, fmt.Errorf("boot emulator: %w", err)
			}
			continue
		}
		if time.Now().After(deadline) {
			return "", nil, nil
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
	}
}

func (m *Manager) tryLease(ctx context.Context) string {
	pool := append([]string{}, m.cfg.Devices.Serials...)
	if m.cfg.Devices.Discover {
		if found, err := m.adbDevices(ctx); err == nil {
			for _, s := range found {
				if !contains(pool, s) {
					pool = append(pool, s)
				}
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range pool {
		if !m.inUse[s] {
			m.inUse[s] = true
			return s
		}
	}
	return ""
}

func (m *Manager) release(serial string) {
	m.mu.Lock()
	booted := m.booted[serial]
	delete(m.inUse, serial)
	m.mu.Unlock()
	if booted && m.cfg.Devices.BootEmulator.KillAfterJob {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _, _ = execx.RunCapture(ctx, execx.Cmd{
			Argv: []string{m.cfg.Devices.AdbBinary, "-s", serial, "emu", "kill"},
		}, 1<<12)
		m.mu.Lock()
		delete(m.booted, serial)
		m.mu.Unlock()
	}
}

// bootEmulator starts the configured AVD detached and waits for boot
// completion, marking the new serial as booted-by-us.
func (m *Manager) bootEmulator(ctx context.Context) error {
	be := m.cfg.Devices.BootEmulator
	if be.AVDName == "" {
		return fmt.Errorf("resources.devices.boot_emulator.avd_name is empty")
	}
	before, _ := m.adbDevices(ctx)
	args := []string{"-avd", be.AVDName}
	if be.Headless {
		args = append(args, "-no-window")
	}
	cmd := exec.Command(be.EmulatorBinary, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	m.mu.Lock()
	m.bootCmd = cmd
	m.mu.Unlock()
	go cmd.Wait() // reap

	deadline := time.Now().Add(be.BootTimeout.D())
	for time.Now().Before(deadline) {
		now, _ := m.adbDevices(ctx)
		for _, s := range now {
			if !contains(before, s) {
				if m.bootCompleted(ctx, s) {
					m.mu.Lock()
					m.booted[s] = true
					m.mu.Unlock()
					return nil
				}
			}
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("emulator %s did not reach boot_completed within %s", be.AVDName, be.BootTimeout)
}

func (m *Manager) bootCompleted(ctx context.Context, serial string) bool {
	_, out, err := execx.RunCapture(ctx, execx.Cmd{
		Argv:    []string{m.cfg.Devices.AdbBinary, "-s", serial, "shell", "getprop", "sys.boot_completed"},
		Timeout: 15 * time.Second,
	}, 1<<10)
	return err == nil && strings.TrimSpace(out) == "1"
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
