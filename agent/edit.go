package agent

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	ErrEditNoMatch = errors.New("the text to replace must appear exactly once in the prompt")
	// ErrNoSettingLine: only top-level lines match, so a "model:" key inside a
	// schema is never touched.
	ErrNoSettingLine = errors.New("frontmatter must have exactly one top-level line for")
)

// PromptBody returns a step's prompt: the part after the frontmatter.
func PromptBody(dir, name string) (string, error) {
	raw, err := readStep(dir, name)
	if err != nil {
		return "", err
	}
	_, body, ok := splitFrontmatter(raw)
	if !ok {
		return "", fmt.Errorf("step %s: missing --- frontmatter", name)
	}
	return body, nil
}

// CheckEdit reports whether EditPrompt would apply, without writing anything.
func CheckEdit(dir, name, find, replace string) error {
	_, err := editedStep(dir, name, find, replace)
	return err
}

// EditPrompt replaces find with replace in a step's prompt. It only ever
// touches the prompt, never the frontmatter (model, tools, permissions,
// budget), so an edit can change what an agent is told but not what it's
// allowed to do. find must appear exactly once, and the result must still
// load, or nothing is written.
func EditPrompt(dir, name, find, replace string) error {
	updated, err := editedStep(dir, name, find, replace)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open steps dir: %w", err)
	}
	defer root.Close()
	if err := root.WriteFile(name+".md", []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write step %s: %w", name, err)
	}
	return nil
}

// SetRunnerModel changes which runner and model a step runs on. It rewrites
// those two lines of the frontmatter and nothing else, so tools, permissions
// and budget stay as reviewed, and the change shows up as a small diff.
func SetRunnerModel(dir, name, runner, model string) error {
	raw, err := readStep(dir, name)
	if err != nil {
		return err
	}
	front, body, ok := splitFrontmatter(raw)
	if !ok {
		return fmt.Errorf("step %s: missing --- frontmatter", name)
	}
	for _, kv := range [][2]string{{"runner", runner}, {"model", model}} {
		line := regexp.MustCompile(`(?m)^` + kv[0] + `:[^\n]*$`)
		if len(line.FindAllStringIndex(front, -1)) != 1 {
			return fmt.Errorf("step %s: %w: %s", name, ErrNoSettingLine, kv[0])
		}
		front = line.ReplaceAllLiteralString(front, kv[0]+": "+kv[1])
	}
	updated := "---\n" + front + "\n---\n" + body
	s, err := parseStep(name, updated)
	if err != nil {
		return fmt.Errorf("change would break step %s: %w", name, err)
	}
	if s.Runner != runner || s.Model != model {
		return fmt.Errorf("step %s: reads back as %s/%s, not %s/%s", name, s.Runner, s.Model, runner, model)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open steps dir: %w", err)
	}
	defer root.Close()
	if err := root.WriteFile(name+".md", []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write step %s: %w", name, err)
	}
	return nil
}

func editedStep(dir, name, find, replace string) (string, error) {
	raw, err := readStep(dir, name)
	if err != nil {
		return "", err
	}
	front, body, ok := splitFrontmatter(raw)
	if !ok {
		return "", fmt.Errorf("step %s: missing --- frontmatter", name)
	}
	if find == "" || strings.Count(body, find) != 1 {
		return "", fmt.Errorf("step %s: %w", name, ErrEditNoMatch)
	}
	updated := "---\n" + front + "\n---\n" + strings.Replace(body, find, replace, 1)
	if _, err := parseStep(name, updated); err != nil {
		return "", fmt.Errorf("edit would break step %s: %w", name, err)
	}
	return updated, nil
}
