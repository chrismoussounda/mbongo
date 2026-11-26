// Command mbongo runs the fake mobile money operator.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chrismoussounda/mbongo/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string, out io.Writer) error {
	logger := slog.New(slog.NewJSONHandler(out, nil))
	srv := &http.Server{
		Addr:              address(getenv),
		Handler:           server.New(logger, version),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Long enough for the slow answers Mbongo will fake on purpose.
		WriteTimeout: 90 * time.Second,
		IdleTimeout:  2 * time.Minute,
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", srv.Addr)
	if err != nil {
		return err
	}
	logger.Info("listening", "addr", listener.Addr().String(), "version", version)

	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return err
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("stopped")
	return nil
}

func address(getenv func(string) string) string {
	if addr := getenv("MBONGO_ADDR"); addr != "" {
		return addr
	}
	if port := getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}
