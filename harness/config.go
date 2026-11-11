// Package harness is the whole pipeline in one program: tickets in a SQLite
// store, an engine that moves each ticket through its steps, the gates that
// stop it, and an HTTP API the UI talks to.
package harness

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"aiharness/target"
)

type Level int

const (
	Low Level = iota
	Medium
	High
)

func (l Level) String() string { return [...]string{"low", "medium", "high"}[l] }

func parseLevel(s string) (Level, error) {
	switch s {
	case "low":
		return Low, nil
	case "medium":
		return Medium, nil
	case "high":
		return High, nil
	}
	return Low, fmt.Errorf("%w: %q", ErrBadLevel, s)
}

var ErrBadLevel = errors.New("risk level must be low, medium or high")

// Config is pipeline.yaml: the rules the harness enforces. Code reads it;
// agents never see it, so they can't argue with it or edit it.
type Config struct {
	MaxRounds int        `yaml:"max_rounds"`
	Risk      RiskConfig `yaml:"risk"`
	Spec      SpecConfig `yaml:"spec"`
	// Models are the ones an agent step may be switched to from the UI.
	Models []string `yaml:"models"`
}

// SpecConfig says when a human approves the spec before the build: when the
// files it plans to change hit a medium or high risk rule (the default),
// always, or never.
type SpecConfig struct {
	Approval string `yaml:"approval"`
}

const (
	SpecRisky  = "risky"
	SpecAlways = "always"
	SpecNever  = "never"
)

var ErrBadApproval = errors.New("spec approval must be risky, always or never")

// RiskConfig is the deterministic risk floor: which paths and diff sizes force
// a human to approve before merge. An LLM risk note could raise the level
// later, but never lower it below this.
type RiskConfig struct {
	MediumLines int        `yaml:"medium_lines"`
	HighLines   int        `yaml:"high_lines"`
	Rules       []RiskRule `yaml:"rules"`
}

type RiskRule struct {
	Paths []string `yaml:"paths"`
	Level string   `yaml:"level"`
	Why   string   `yaml:"why"`
	level Level
}

func LoadConfig(file string) (Config, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // operator-supplied config path
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", file, err)
	}
	if c.MaxRounds <= 0 {
		c.MaxRounds = 3
	}
	switch c.Spec.Approval {
	case "":
		c.Spec.Approval = SpecRisky
	case SpecRisky, SpecAlways, SpecNever:
	default:
		return Config{}, fmt.Errorf("%s: %w: %q", file, ErrBadApproval, c.Spec.Approval)
	}
	for i := range c.Risk.Rules {
		r := &c.Risk.Rules[i]
		if r.level, err = parseLevel(r.Level); err != nil {
			return Config{}, fmt.Errorf("%s: rule %d: %w", file, i+1, err)
		}
		for _, p := range r.Paths {
			if _, err := path.Match(p, ""); err != nil {
				return Config{}, fmt.Errorf("%s: rule %d: bad pattern %q: %w", file, i+1, p, err)
			}
		}
	}
	return c, nil
}

// Assess returns the highest level any rule or size threshold reaches, and
// the reasons, so a human sees why they're being asked.
func (c RiskConfig) Assess(changes []target.FileChange) (Level, []string) {
	level := Low
	var why []string
	raise := func(l Level, reason string) {
		level = max(level, l)
		why = append(why, l.String()+": "+reason)
	}
	lines := 0
	for _, ch := range changes {
		lines += ch.Added + ch.Deleted
		for _, r := range c.Rules {
			if matchesAny(r.Paths, ch.Path) {
				raise(r.level, fmt.Sprintf("%s (%s)", ch.Path, r.Why))
			}
		}
	}
	switch {
	case c.HighLines > 0 && lines >= c.HighLines:
		raise(High, fmt.Sprintf("%d lines changed (high from %d)", lines, c.HighLines))
	case c.MediumLines > 0 && lines >= c.MediumLines:
		raise(Medium, fmt.Sprintf("%d lines changed (medium from %d)", lines, c.MediumLines))
	}
	if len(why) == 0 {
		why = append(why, fmt.Sprintf("low: %d files, %d lines, no risky paths", len(changes), lines))
	}
	return level, why
}

// matchesAny matches a pattern against the full path, or against the file
// name alone for patterns without a slash ("*.sql" matches db/x.sql).
func matchesAny(patterns []string, file string) bool {
	for _, p := range patterns {
		target := file
		if !strings.Contains(p, "/") {
			target = path.Base(file)
		}
		if ok, _ := path.Match(p, target); ok {
			return true
		}
	}
	return false
}
