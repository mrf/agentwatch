// Command stdout is an example that prints agentwatch session updates to stdout.
// It discovers Claude Code sessions in a configurable directory and prints each
// event as JSON. This is the hello-world vertical slice for agentwatch.
//
// Usage:
//
//	stdout -dir ~/.claude/projects
//	AGENTWATCH_DIR=~/.claude/projects stdout
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/sources/claude"
)

func main() {
	dir := flag.String("dir", "", "Claude projects directory (default: $AGENTWATCH_DIR)")
	flag.Parse()

	if *dir == "" {
		*dir = os.Getenv("AGENTWATCH_DIR")
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: stdout -dir <path>  or  AGENTWATCH_DIR=<path> stdout")
		os.Exit(1)
	}

	src, err := claude.New(claude.WithRoot(*dir))
	if err != nil {
		log.Fatalf("create source: %v", err)
	}

	sink := monitor.EventSinkFunc(func(_ context.Context, ev monitor.Event) error {
		data, err := json.MarshalIndent(ev, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	})

	mon, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
	)
	if err != nil {
		log.Fatalf("create monitor: %v", err)
	}

	ctx := context.Background()
	if err := mon.PollOnce(ctx); err != nil {
		log.Fatalf("poll: %v", err)
	}

	sessions := mon.Snapshot()
	fmt.Printf("\n--- snapshot: %d session(s) ---\n", len(sessions))
	for i := 0; i < len(sessions); i++ {
		data, err := json.MarshalIndent(sessions[i], "", "  ")
		if err != nil {
			log.Printf("marshal session: %v", err)
			continue
		}
		fmt.Println(string(data))
	}
}
