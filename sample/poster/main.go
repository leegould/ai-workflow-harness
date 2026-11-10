package main

import (
	"flag"
	"fmt"
	"log"
	"os"
)

func main() {
	in := flag.String("in", "poster.json", "poster content")
	out := flag.String("out", "poster.html", "where to write the page")
	flag.Parse()

	if err := run(*in, *out); err != nil {
		log.Fatalln(err)
	}
}

func run(in, out string) error {
	p, err := Load(in)
	if err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	if err := Render(f, p); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}
