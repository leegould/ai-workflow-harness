package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Sandboxes a step file can ask for with `sandbox:`. A sandbox contains what
// an agent's shell commands can reach, which matters once ticket text can come
// from outside (a Sentry report, a webhook): a prompt injection that gets the
// agent to run something still can't read your home folder or send it
// anywhere. It covers shell commands and what they start (go test runs code
// the agent wrote); the file tools are confined to the checkout separately.
const (
	SandboxNone = "none"
	// SandboxNative is the runner's own OS sandbox: for Claude Code, Seatbelt
	// on macOS and bubblewrap on Linux.
	SandboxNative = "native"
	// SandboxOpenSandbox runs the step in a container through OpenSandbox. No
	// runner supports it yet: it needs Docker and the OpenSandbox server.
	SandboxOpenSandbox = "opensandbox"
)

var Sandboxes = []string{SandboxNone, SandboxNative, SandboxOpenSandbox}

var ErrSandboxUnavailable = errors.New("sandbox not available")

// SandboxPolicy is what a sandboxed step may reach, from pipeline.yaml. It's
// the same for every sandboxed step, and step files can't widen it.
type SandboxPolicy struct {
	// Network is the hosts sandboxed commands may connect to. Anything else is
	// refused, not asked about: nobody is there to answer.
	Network []string `yaml:"network"`
	// CacheDir is where sandboxed commands keep build caches (Go's build and
	// module caches, and XDG_CACHE_HOME), instead of your own. Sharing yours
	// would let a sandboxed build plant cache entries your normal builds use.
	CacheDir string `yaml:"cache_dir"`
}

// NativeSandbox reports the OS sandbox this machine has, by name, or why it
// has none. Claude Code needs nothing installed on macOS; on Linux it needs
// bubblewrap and socat.
func NativeSandbox() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("sandbox-exec"); err != nil {
			return "", fmt.Errorf("%w: sandbox-exec (Seatbelt) not found", ErrSandboxUnavailable)
		}
		return "Seatbelt", nil
	case "linux":
		for _, bin := range []string{"bwrap", "socat"} {
			if _, err := exec.LookPath(bin); err != nil {
				return "", fmt.Errorf("%w: install bubblewrap and socat (%s not found)", ErrSandboxUnavailable, bin)
			}
		}
		return "bubblewrap", nil
	}
	return "", fmt.Errorf("%w: no OS sandbox on %s", ErrSandboxUnavailable, runtime.GOOS)
}

// claudeSandbox is the --settings JSON that turns on Claude Code's sandbox
// with policy p, and the environment the claude process gets so build tools
// use the sandbox's own caches. failIfUnavailable makes claude exit rather
// than run unsandboxed, and allowUnsandboxedCommands=false stops the agent
// retrying a refused command outside the sandbox.
func claudeSandbox(p SandboxPolicy) (settings string, env []string, err error) {
	s := map[string]any{
		"enabled":                  true,
		"failIfUnavailable":        true,
		"allowUnsandboxedCommands": false,
		"network": map[string]any{
			"allowedDomains":  nonNil(p.Network),
			"strictAllowlist": true,
		},
	}
	env = os.Environ()
	if p.CacheDir != "" {
		cache, err := filepath.Abs(p.CacheDir)
		if err != nil {
			return "", nil, fmt.Errorf("sandbox cache dir: %w", err)
		}
		if err := os.MkdirAll(cache, 0o750); err != nil {
			return "", nil, fmt.Errorf("sandbox cache dir: %w", err)
		}
		s["filesystem"] = map[string]any{"allowWrite": []string{cache}}
		env = append(env,
			"GOCACHE="+filepath.Join(cache, "go-build"),
			"GOMODCACHE="+filepath.Join(cache, "go-mod"),
			"XDG_CACHE_HOME="+cache,
			// The module cache is read-only by default, which stops the
			// harness clearing it.
			"GOFLAGS=-modcacherw",
		)
	}
	raw, err := json.Marshal(map[string]any{"sandbox": s})
	if err != nil {
		return "", nil, fmt.Errorf("sandbox settings: %w", err)
	}
	return string(raw), env, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
