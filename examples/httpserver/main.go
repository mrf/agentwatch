// Command httpserver is an example that serves agentwatch session state over
// HTTP and WebSocket. It combines the httpapi and wsapi transports on a single
// port, driven by one or more claude source directories.
//
// Usage:
//
//	go run ./examples/httpserver -dir ~/.claude/projects
//
// Flags:
//
//	-addr  listen address (default 127.0.0.1:8080)
//	-dir   source directory to scan, repeatable
//	-poll  source poll interval (default 2s)
//
// Endpoints:
//
//	GET  /sessions      – list all sessions (JSON)
//	GET  /sessions/{id} – single session by ID (JSON)
//	GET  /healthz       – source health (JSON)
//	GET  /sources       – registered source names (JSON)
//	GET  /ws            – WebSocket event stream
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/claude"
	"github.com/mrf/agentwatch/transport/httpapi"
	"github.com/mrf/agentwatch/transport/wsapi"
)

// dirList is a flag.Value that accumulates multiple -dir flags.
type dirList []string

func (d *dirList) String() string { return strings.Join(*d, ",") }
func (d *dirList) Set(v string) error {
	*d = append(*d, v)
	return nil
}

// sinkRelay breaks the monitor↔wsapi circular dependency. Pass it to
// monitor.New as the sink, then call set once the wsapi.Server is ready.
type sinkRelay struct {
	mu     sync.Mutex
	target monitor.EventSink
}

func (r *sinkRelay) set(s monitor.EventSink) {
	r.mu.Lock()
	r.target = s
	r.mu.Unlock()
}

func (r *sinkRelay) HandleEvent(ctx context.Context, ev monitor.Event) error {
	r.mu.Lock()
	s := r.target
	r.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.HandleEvent(ctx, ev)
}

func main() {
	var dirs dirList
	addr := flag.String("addr", "127.0.0.1:8080", "listen address (host:port)")
	flag.Var(&dirs, "dir", "source directory to scan (repeatable)")
	poll := flag.Duration("poll", 2*time.Second, "poll interval")
	flag.Parse()

	if len(dirs) == 0 {
		log.Fatal("at least one -dir is required")
	}

	// Build one claude source per configured directory.
	sources := make([]source.Source, 0, len(dirs))
	for i := 0; i < len(dirs); i++ {
		src, err := claude.New(claude.WithRoot(dirs[i]))
		if err != nil {
			log.Fatalf("claude.New(%q): %v", dirs[i], err)
		}
		sources = append(sources, src)
	}

	// Break the monitor↔wsapi circular dependency: monitor needs wsapi as its
	// event sink, and wsapi needs the monitor to send initial snapshots. The
	// relay is set before Run starts, so no events are dropped.
	relay := &sinkRelay{}

	mon, err := monitor.New(
		monitor.WithSources(sources...),
		monitor.WithPollInterval(*poll),
		monitor.WithSink(relay),
	)
	if err != nil {
		log.Fatalf("monitor.New: %v", err)
	}

	wsSrv, err := wsapi.NewServer(mon)
	if err != nil {
		log.Fatalf("wsapi.NewServer: %v", err)
	}
	relay.set(wsSrv) // wire the relay before the monitor starts

	httpHandler, err := httpapi.NewHandler(mon)
	if err != nil {
		log.Fatalf("httpapi.NewHandler: %v", err)
	}

	// Mount both transports. /ws is explicit; everything else falls through
	// to the httpapi handler which owns /sessions, /healthz, and /sources.
	mux := http.NewServeMux()
	mux.Handle("/ws", wsSrv)
	mux.Handle("/", httpHandler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Print("shutting down...")
		cancel()
		wsSrv.Stop()

		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
	}()

	// Poll sources on the configured interval, delivering events to the wsapi
	// server via the relay sink.
	go func() {
		if err := mon.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("monitor.Run: %v", err)
		}
	}()

	log.Printf("listening on http://%s", *addr)
	log.Printf("  GET  /sessions      – list all sessions")
	log.Printf("  GET  /sessions/{id} – single session")
	log.Printf("  GET  /healthz       – source health")
	log.Printf("  GET  /sources       – registered source names")
	log.Printf("  GET  /ws            – WebSocket event stream")

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
