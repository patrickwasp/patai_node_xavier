package main

// Tailscale, for the console (docs/console.md, Remote access with Tailscale). The unit's tailscaled starts only while
// tailscaleOn is there, and its env file says whether its logs go to Tailscale. The console talks to tailscaled itself,
// over its socket, once it's tailscaled's operator, which root makes it each time it starts it.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"patai/console/internal/durable"
)

const (
	tailscaleDir    = "/persist/tailscale"
	tailscaleOn     = tailscaleDir + "/on"
	tailscaleEnv    = tailscaleDir + "/env"
	tailscaleState  = tailscaleDir + "/state"
	tailscaleSocket = "/run/tailscale/tailscaled.sock"
	tailscaleCLI    = "/usr/local/bin/tailscale"
	// The console's account, which tailscaled lets change its settings as its operator.
	consoleUser = "patai-api"
)

// tailscaleEnvFile is what tailscaled starts with: its logs kept to the unit unless the owner sends them, and its log
// configuration on /persist, not the slot.
func tailscaleEnvFile(logs bool) []byte {
	return fmt.Appendf(nil, "TS_NO_LOGS_NO_SUPPORT=%t\nTS_LOGS_DIR=%s/logs\n", !logs, tailscaleDir)
}

// tailscaleStart turns Tailscale on, or starts it again with its logs sent or not: the marker and the env file are
// written durably first, so a power cut leaves it as the owner asked. It answers once the console can change it, and
// tailscaled is set not to put its own firewall chains first, which would accept the tailnet ahead of patai-firewall.
func tailscaleStart(logs bool) (string, error) {
	if err := exec.Command("mountpoint", "-q", "/persist").Run(); err != nil {
		return "", errors.New("/persist isn't mounted")
	}
	if _, err := os.Stat("/usr/local/sbin/tailscaled"); err != nil {
		return "", errors.New("this release has no Tailscale")
	}
	for _, dir := range []string{tailscaleDir, tailscaleState, tailscaleDir + "/logs"} {
		mode := os.FileMode(0o700)
		if dir == tailscaleDir {
			// The console reads whether it's on; what's inside stays root's.
			mode = 0o755
		}
		if err := os.MkdirAll(dir, mode); err != nil {
			return "", err
		}
		if err := os.Chmod(dir, mode); err != nil {
			return "", err
		}
	}
	if err := durable.Write(tailscaleEnv, tailscaleEnvFile(logs), 0o600); err != nil {
		return "", err
	}
	if err := durable.Write(tailscaleOn, nil, 0o644); err != nil {
		return "", err
	}
	if _, err := run(time.Minute, "systemctl", "restart", "tailscaled"); err != nil {
		return "", err
	}
	if err := waitForSocket(tailscaleSocket, 30*time.Second); err != nil {
		return "", err
	}
	return run(30*time.Second, tailscaleCLI, "--socket="+tailscaleSocket, "set", "--operator="+consoleUser,
		"--netfilter-mode=nodivert")
}

// tailscaleStop turns it off. The login stays on /persist for when it's turned on again.
func tailscaleStop() (string, error) {
	if err := os.Remove(tailscaleOn); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := durable.SyncDir(tailscaleDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return run(time.Minute, "systemctl", "stop", "tailscaled")
}

// tailscaleSync flushes what tailscaled wrote: it syncs a file before renaming it, but not the directory after, so a
// state file new since the last sync could come back empty after a power cut (internal/durable). The console asks once
// a login has finished.
func tailscaleSync() (string, error) {
	err := filepath.WalkDir(tailscaleState, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return durable.SyncDir(path)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return "", durable.SyncDir(tailscaleDir)
}

// tailscaleLogout takes the unit off its tailnet, if it's on one and can say so: before a factory reset, which finishes
// early in the next boot, with no network. It's best done, never a reason not to reset.
func tailscaleLogout() {
	if _, err := os.Stat(tailscaleOn); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, tailscaleCLI, "--socket="+tailscaleSocket, "logout").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "tailscale logout before the reset: %v: %s\n", err, out)
	}
}

// waitForSocket waits until something answers on a unix socket.
func waitForSocket(path string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tailscaled didn't start: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
