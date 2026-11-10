// Command harness runs the whole pipeline as one program: open the UI, create
// a ticket, and watch it go from readiness to a deployed change.
//
//	go run ./cmd/harness        # then open http://localhost:8080
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"aiharness/agent"
	"aiharness/harness"
	"aiharness/target"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "where to serve the UI and API")
	db := flag.String("db", ".harness/harness.db", "ticket store (SQLite)")
	targetDir := flag.String("target", target.DefaultDir(), "target workspace: the local stand-in for GitHub and deploy")
	seed := flag.String("seed", "sample/poster", "initial contents of the target repo, used on first run")
	steps := flag.String("steps", "steps", "agent step definitions")
	config := flag.String("config", "pipeline.yaml", "pipeline rules: rounds and the risk floor")
	workers := flag.Int("workers", 2, "tickets worked on at once")
	flag.Parse()

	if err := run(*addr, *db, *targetDir, *seed, *steps, *config, *workers); err != nil {
		log.Fatalln(err)
	}
}

func run(addr, db, targetDir, seed, steps, config string, workers int) error {
	cfg, err := harness.LoadConfig(config)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	local := target.Local{Dir: targetDir}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !local.Exists() {
		if err := local.Init(ctx, seed); err != nil {
			return fmt.Errorf("create target: %w", err)
		}
		log.Printf("created target repo %s from %s", local.Origin(), seed)
	}
	if err := os.MkdirAll(filepath.Dir(db), 0o750); err != nil {
		return fmt.Errorf("store dir: %w", err)
	}
	store, err := harness.Open(db)
	if err != nil {
		return fmt.Errorf("open %s: %w", db, err)
	}
	defer store.Close()

	engine := &harness.Engine{
		Store:    store,
		Repo:     local,
		Runner:   agent.Runners{"claude-cli": agent.ClaudeCLI{}},
		StepsDir: steps,
		Config:   cfg,
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           harness.Server(engine, filepath.Join(local.DeployDir(), "poster.html")),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	go func() {
		if err := engine.Run(ctx, workers); err != nil {
			log.Printf("engine: %v", err)
		}
	}()

	log.Printf("harness on http://%s (target %s)", addr, local.Dir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
