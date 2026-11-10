package target

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Init creates the workspace: origin.git seeded from the seed directory.
func (l Local) Init(ctx context.Context, seed string) error {
	if _, err := os.Stat(l.Origin()); err == nil {
		return fmt.Errorf("%s already exists", l.Origin())
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check origin: %w", err)
	}
	if _, err := git(ctx, "", "init", "-q", "--bare", "-b", "main", l.Origin()); err != nil {
		return err
	}

	src, err := os.MkdirTemp("", "aiw-seed-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(src) }()
	if err := os.CopyFS(src, os.DirFS(seed)); err != nil {
		return fmt.Errorf("copy seed: %w", err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.name=" + gitName, "-c", "user.email=" + gitEmail, "commit", "-q", "-m", "Initial poster"},
		{"push", "-q", l.Origin(), "main"},
	} {
		if _, err := git(ctx, src, args...); err != nil {
			return err
		}
	}

	return nil
}

// Exists reports whether the workspace has been created.
func (l Local) Exists() bool {
	_, err := os.Stat(l.Origin())
	return err == nil
}
