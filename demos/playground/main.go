// Command playground serves a local page for trying dalforge's IDL: edit a
// proto file and see the lint findings and everything dalforge and sqlc
// generate from it.
//
// It drives the dalforge CLI the way a user's project does, so it needs
// dalforge and sqlc on PATH; `mise run playground` from the dalforge
// repository provides both.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7070", "address to listen on; keep it on loopback, since the page runs dalforge and sqlc on whatever it's sent")
	dalforge := flag.String("dalforge", "dalforge", "the dalforge binary to run (a name on PATH, or a path)")
	flag.Parse()
	if err := run(*addr, *dalforge); err != nil {
		fmt.Fprintln(os.Stderr, "playground:", err)
		os.Exit(1)
	}
}

func run(addr, dalforge string) error {
	bin, err := exec.LookPath(dalforge)
	if err != nil {
		return fmt.Errorf("%w (install dalforge, or run `mise run playground` in the dalforge repository)", err)
	}
	if _, err := exec.LookPath("sqlc"); err != nil {
		fmt.Fprintln(os.Stderr, "playground: sqlc isn't on PATH; dalforge generate will report it on every run")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	handler, err := NewServer(ctx, bin)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "playground: %s on http://%s (Ctrl+C to stop)\n", handler.version, ln.Addr())

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
