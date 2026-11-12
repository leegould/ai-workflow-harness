// Package target is the local stand-in for GitHub, CI and the deploy target,
// so the harness can run a real ticket end to end without a hosted repo:
//
//   - origin.git: a bare repo playing GitHub. Branches are PRs.
//   - work/<key>: agents' checkouts.
//   - deploy/: the "production" poster binary and the page it renders.
//
// The harness only sees it through its Repo interface, so a GitHub-backed
// version can replace it.
package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	gitName  = "ai-pipeline"
	gitEmail = "ai-pipeline@localhost"
	// logTail is how much CI or smoke output goes back to the workflow, and on
	// to the build agent as feedback. The end of the output is what matters.
	logTail = 4000
)

// Local is one target workspace on disk.
type Local struct {
	Dir string
}

type CIResult struct {
	SHA    string
	Passed bool
	Log    string
}

type SmokeResult struct {
	Passed bool
	Log    string
}

// FileChange is one file in a branch's diff against main.
type FileChange struct {
	Path           string
	Added, Deleted int
}

// DefaultDir is $AIW_TARGET, or ~/.cache/ai-workflow-harness-target. It sits outside
// any project folder on purpose: Claude Code reads CLAUDE.md files from parent
// directories, and the build agent should only see the target repo's rules.
func DefaultDir() string {
	if d := os.Getenv("AIW_TARGET"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "ai-workflow-harness-target")
	}
	return filepath.Join(home, ".cache", "ai-workflow-harness-target")
}

func (l Local) Origin() string { return filepath.Join(l.Dir, "origin.git") }

// Environments a change is deployed to: every merge goes to dev, and
// production gets a commit that has already passed there.
const (
	Dev        = "dev"
	Production = "production"
)

var ErrUnknownEnv = errors.New("unknown environment")

// DeployDir is where env's running version lives.
func (l Local) DeployDir(env string) string { return filepath.Join(l.Dir, "deploy", env) }

func checkEnv(env string) error {
	if env != Dev && env != Production {
		return fmt.Errorf("%w: %q", ErrUnknownEnv, env)
	}
	return nil
}

// Checkout gives an agent a clean checkout of branch in work/<key>, starting
// from main if the branch doesn't exist yet. Anything left over from an
// earlier attempt is wiped, so a retried activity starts from what was pushed.
func (l Local) Checkout(ctx context.Context, key, branch string) (string, error) {
	dir := filepath.Join(l.Dir, "work", key)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if _, err := git(ctx, "", "clone", "-q", l.Origin(), dir); err != nil {
			return "", err
		}
	}
	if _, err := git(ctx, dir, "fetch", "-q", "--prune", "origin"); err != nil {
		return "", err
	}
	base := "origin/main"
	if _, err := git(ctx, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch); err == nil {
		base = "origin/" + branch
	}
	for _, args := range [][]string{
		{"checkout", "-q", "-B", branch, base},
		{"reset", "-q", "--hard", base},
		{"clean", "-q", "-fdx"},
	} {
		if _, err := git(ctx, dir, args...); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// CommitAndPush commits whatever the agent changed and pushes it. An agent
// that changed nothing still gets a commit, so the round runs
// through CI and review like any other and the cap still applies.
func (l Local) CommitAndPush(ctx context.Context, dir, branch, message string) (string, error) {
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.name=" + gitName, "-c", "user.email=" + gitEmail, "commit", "-q", "--allow-empty", "-m", message},
		{"push", "-q", "origin", "HEAD:refs/heads/" + branch},
	} {
		if _, err := git(ctx, dir, args...); err != nil {
			return "", err
		}
	}
	return git(ctx, dir, "rev-parse", "HEAD")
}

func (l Local) HeadSHA(ctx context.Context, branch string) (string, error) {
	return git(ctx, "", "--git-dir", l.Origin(), "rev-parse", "refs/heads/"+branch)
}

// Merge merges branch into main with a merge commit and returns its SHA.
func (l Local) Merge(ctx context.Context, branch, message string) (string, error) {
	return l.onMain(ctx, []string{"merge", "-q", "--no-ff", "-m", message, "origin/" + branch})
}

// Revert undoes a merge commit on main and returns the new head.
func (l Local) Revert(ctx context.Context, mergeSHA string) (string, error) {
	return l.onMain(ctx, []string{"revert", "--no-edit", "-m", "1", mergeSHA})
}

// onMain runs one git command on a fresh clone of main and pushes the result.
func (l Local) onMain(ctx context.Context, args []string) (string, error) {
	dir, cleanup, err := l.clone(ctx, "main")
	if err != nil {
		return "", err
	}
	defer cleanup()
	identity := []string{"-c", "user.name=" + gitName, "-c", "user.email=" + gitEmail}
	if _, err := git(ctx, dir, append(identity, args...)...); err != nil {
		return "", err
	}
	if _, err := git(ctx, dir, "push", "-q", "origin", "HEAD:refs/heads/main"); err != nil {
		return "", err
	}
	return git(ctx, dir, "rev-parse", "HEAD")
}

// Deploy builds ref (a branch or commit) and swaps it in as env's running
// version. It returns the deployed commit.
func (l Local) Deploy(ctx context.Context, env, ref string) (string, error) {
	if err := checkEnv(env); err != nil {
		return "", err
	}
	src, cleanup, err := l.clone(ctx, ref)
	if err != nil {
		return "", err
	}
	defer cleanup()
	sha, err := git(ctx, src, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	dst := l.DeployDir(env)
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return "", fmt.Errorf("deploy dir: %w", err)
	}
	next := filepath.Join(dst, "poster.next")
	if out, err := run(ctx, src, "go", "build", "-o", next, "."); err != nil {
		return "", fmt.Errorf("build %s: %w\n%s", sha, err, out)
	}
	content, err := os.ReadFile(filepath.Join(src, "poster.json")) //nolint:gosec // our own clone
	if err != nil {
		return "", fmt.Errorf("read poster.json: %w", err)
	}
	for _, f := range []struct {
		name string
		data string
	}{{"poster.json", string(content)}, {"DEPLOYED", sha + "\n"}} {
		if err := os.WriteFile(filepath.Join(dst, f.name), []byte(f.data), 0o600); err != nil {
			return "", fmt.Errorf("deploy %s: %w", f.name, err)
		}
	}
	// Rename is atomic, so nothing ever runs a half-written binary.
	if err := os.Rename(next, filepath.Join(dst, "poster")); err != nil {
		return "", fmt.Errorf("swap binary: %w", err)
	}
	return sha, nil
}

// Contains reports whether commit includes ancestor, i.e. whether deploying
// commit also deploys ancestor.
func (l Local) Contains(ctx context.Context, commit, ancestor string) (bool, error) {
	_, err := run(ctx, "", "git", "--git-dir", l.Origin(), "merge-base", "--is-ancestor", ancestor, commit)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("git merge-base: %w", err)
	}
	return true, nil
}

// Deployed returns the commit running in env, or "" if nothing is.
func (l Local) Deployed(_ context.Context, env string) (string, error) {
	if err := checkEnv(env); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(l.DeployDir(env), "DEPLOYED"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read deployed: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// Smoke runs env's deployed binary the way the README says to and checks it
// produced a page.
func (l Local) Smoke(ctx context.Context, env string) (SmokeResult, error) {
	if err := checkEnv(env); err != nil {
		return SmokeResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dst := l.DeployDir(env)
	page := filepath.Join(dst, "poster.html")
	_ = os.Remove(page)
	out, err := run(ctx, dst, "./poster", "-in", "poster.json", "-out", "poster.html")
	if err != nil {
		return SmokeResult{Log: fmt.Sprintf("poster exited: %v\n%s", err, out)}, nil
	}
	html, err := os.ReadFile(page) //nolint:gosec // fixed path in the deploy dir
	if err != nil {
		return SmokeResult{Log: "no page written: " + err.Error()}, nil
	}
	if !bytes.Contains(html, []byte("</html>")) {
		return SmokeResult{Log: "page is not a complete HTML document"}, nil
	}
	return SmokeResult{Passed: true, Log: page}, nil
}

// RunCI checks out sha and runs the target's checks. These live here, not in
// the target repo, so the agent can't loosen the gate it's judged by. A
// failing check is a result for the builder; failing to run the checks at all
// is an error, an infrastructure problem that must not go back to the agent.
func (l Local) RunCI(ctx context.Context, sha string) (CIResult, error) {
	res := CIResult{SHA: sha}
	dir, cleanup, err := l.clone(ctx, sha)
	if err != nil {
		return res, err
	}
	defer cleanup()

	var log strings.Builder
	passed := true
	for _, c := range [][]string{
		{"gofmt", "-l", "."},
		{"go", "vet", "./..."},
		{"go", "test", "./..."},
	} {
		out, err := run(ctx, dir, c[0], c[1:]...)
		fmt.Fprintf(&log, "$ %s\n%s", strings.Join(c, " "), out)
		// gofmt -l exits 0 and lists unformatted files.
		if err != nil || (c[0] == "gofmt" && strings.TrimSpace(out) != "") {
			passed = false
			fmt.Fprintf(&log, "FAILED: %s\n", strings.Join(c, " "))
			break
		}
	}
	res.Passed = passed
	res.Log = tail(log.String(), logTail)
	return res, nil
}

// ChangedFiles lists what branch changes relative to where it left main.
func (l Local) ChangedFiles(ctx context.Context, branch string) ([]FileChange, error) {
	out, err := git(ctx, "", "--git-dir", l.Origin(), "diff", "--numstat", "refs/heads/main...refs/heads/"+branch)
	if err != nil {
		return nil, err
	}
	var changes []FileChange
	for line := range strings.Lines(out) {
		f := strings.SplitN(strings.TrimSpace(line), "\t", 3)
		if len(f) != 3 {
			continue
		}
		// Binary files show "-" for both counts.
		added, _ := strconv.Atoi(f[0])
		deleted, _ := strconv.Atoi(f[1])
		changes = append(changes, FileChange{Path: f[2], Added: added, Deleted: deleted})
	}
	return changes, nil
}

// clone makes a throwaway clone of origin at rev.
func (l Local) clone(ctx context.Context, rev string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "aiw-")
	if err != nil {
		return "", nil, fmt.Errorf("temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if _, err := git(ctx, "", "clone", "-q", l.Origin(), dir); err != nil {
		cleanup()
		return "", nil, err
	}
	if _, err := git(ctx, dir, "checkout", "-q", rev); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := run(ctx, dir, "git", args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out), nil
}

// run executes a command with GIT_* variables removed: inside a git hook they
// point at the hook's repo and would hijack every git command we run.
func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed commands; args are ours or git refs
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
