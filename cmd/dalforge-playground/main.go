// Command dalforge-playground serves a local page for trying dalforge's IDL:
// edit a proto file and see the lint findings and everything dalforge and
// sqlc generate from it. Run it with `mise run playground`, which puts the
// pinned sqlc on PATH.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gisripa/dalforge/internal/pipeline"
	"github.com/gisripa/dalforge/internal/playground"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7070", "address to listen on; keep it on loopback, since the page runs sqlc on whatever it's sent")
	flag.Parse()
	if err := run(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "dalforge-playground:", err)
		os.Exit(1)
	}
}

func run(addr string) error {
	sqlc, err := pipeline.FindSQLC()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dalforge-playground: sqlc not found; showing dalforge's output only:", err)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           playground.New(sqlc),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "dalforge playground: http://%s (Ctrl+C to stop)\n", ln.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
