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
	"strconv"
	"syscall"
	"time"

	"github.com/chrismoussounda/mbongo/internal/admin"
	"github.com/chrismoussounda/mbongo/internal/airtel"
	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
	"github.com/chrismoussounda/mbongo/internal/faults"
	"github.com/chrismoussounda/mbongo/internal/janitor"
	"github.com/chrismoussounda/mbongo/internal/money"
	"github.com/chrismoussounda/mbongo/internal/mtn"
	"github.com/chrismoussounda/mbongo/internal/phone"
	"github.com/chrismoussounda/mbongo/internal/report"
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
	if err := autoCustomers(getenv, books); err != nil {
		return err
	}

	rules, err := faults.New(ctx, books.DB())
	if err != nil {
		return err
	}
	journal, err := report.New(ctx, books.DB(), books.Clock())
	if err != nil {
		return err
	}
	momo, err := mtn.New(ctx, books, logger, nil)
	if err != nil {
		return err
	}
	momo.WithFaults(rules).WithReport(journal)
	defer momo.Wait()

	airtelMoney, err := airtel.New(ctx, books, logger, nil)
	if err != nil {
		return err
	}
	defer airtelMoney.Wait()
	airtelMoney.WithFaults(rules).WithReport(journal)

	phones, err := phone.New(ctx, books, map[bank.Operator]phone.Operator{bank.MTN: momo, bank.Airtel: airtelMoney}, logger)
	if err != nil {
		return err
	}

	sweepCtx, stopSweep := context.WithCancel(ctx)
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		janitor.Run(sweepCtx, time.Second, func(err error) { logger.Error("housekeeping", "err", err) },
			func(ctx context.Context) error { _, err := books.ExpireDue(ctx); return err },
			momo.PurgeTokens, airtelMoney.PurgeTokens, journal.Prune)
	}()
	defer func() { stopSweep(); <-swept }()

	srv := &http.Server{
		Addr: address(getenv),
		Handler: server.New(logger, version, books, momo.Routes, airtelMoney.Routes, func(mux *http.ServeMux) {
			admin.Routes(mux, getenv("MBONGO_ADMIN_TOKEN"), books, momo, airtelMoney, rules, phones, journal, logger)
		}, func(mux *http.ServeMux) {
			phones.Routes(ctx, mux)
		}, func(mux *http.ServeMux) {
			journal.Routes(mux, map[string]report.Ledger{"mtn": momo, "airtel": airtelMoney}, logger)
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

// autoCustomers reads MBONGO_AUTO_CUSTOMERS and MBONGO_AUTO_BALANCE: with the first true, any
// number a call uses becomes a customer holding the second, 100000 XAF by default.
func autoCustomers(getenv func(string) string, books *bank.Bank) error {
	raw := getenv("MBONGO_AUTO_CUSTOMERS")
	if raw == "" {
		return nil
	}
	on, err := strconv.ParseBool(raw)
	if err != nil {
		return fmt.Errorf("MBONGO_AUTO_CUSTOMERS must be true or false, not %q", raw)
	}
	if !on {
		return nil
	}
	balance := int64(100_000)
	if raw := getenv("MBONGO_AUTO_BALANCE"); raw != "" {
		if balance, err = money.Parse(raw, "XAF"); err != nil {
			return fmt.Errorf("MBONGO_AUTO_BALANCE must be an amount in XAF, such as 100000, not %q", raw)
		}
	}
	books.AutoCustomers(balance)
	return nil
}
