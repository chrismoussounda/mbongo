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

	"github.com/chrismoussounda/mbongo/internal/admin"
	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
	"github.com/chrismoussounda/mbongo/internal/janitor"
	"github.com/chrismoussounda/mbongo/internal/mtn"
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
	path := getenv("MBONGO_DB")
	if path == "" {
		path = "mbongo.db"
	}
	books, err := bank.Open(ctx, path, clock.Real{})
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = books.Close() }()

	momo, err := mtn.New(ctx, books, logger, nil)
	if err != nil {
		return err
	}
	defer momo.Wait()

	sweepCtx, stopSweep := context.WithCancel(ctx)
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		janitor.Run(sweepCtx, time.Second, func(err error) { logger.Error("housekeeping", "err", err) },
			func(ctx context.Context) error { _, err := books.ExpireDue(ctx); return err },
			momo.PurgeTokens)
	}()
	defer func() { stopSweep(); <-swept }()

	srv := &http.Server{
		Addr: address(getenv),
		Handler: server.New(logger, version, books, momo.Routes, func(mux *http.ServeMux) {
			admin.Routes(mux, getenv("MBONGO_ADMIN_TOKEN"), books, momo, logger)
		}),
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
