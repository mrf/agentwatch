// Command httpserver is an example that serves agentwatch session state over HTTP.
//
// Usage:
//
//	go run ./examples/httpserver
//
// Endpoints:
//
//	GET http://127.0.0.1:8080/sessions      - list all sessions
//	GET http://127.0.0.1:8080/sessions/{id} - single session
//	GET http://127.0.0.1:8080/healthz       - source health
//	GET http://127.0.0.1:8080/sources       - registered source names
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/sources/claude"
	"github.com/mrf/agentwatch/transport/httpapi"
)

func main() {
	src, err := claude.New()
	if err != nil {
		log.Fatalf("claude.New: %v", err)
	}

	mon, err := monitor.New(monitor.WithSources(src))
	if err != nil {
		log.Fatalf("monitor.New: %v", err)
	}

	handler, err := httpapi.NewHandler(mon)
	if err != nil {
		log.Fatalf("httpapi.NewHandler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if err := mon.PollOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("PollOnce: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	addr := "127.0.0.1:8080"
	log.Printf("listening on http://%s", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
