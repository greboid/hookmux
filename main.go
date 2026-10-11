package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/csmith/envflag"
	"github.com/csmith/middleware"
	"github.com/csmith/slogflags"

	"github.com/greboid/hookmux/internal/record"
	"github.com/greboid/hookmux/internal/routes"
	"github.com/greboid/hookmux/internal/server"
)

var (
	listenAddress = flag.String("listen", ":8080", "Address to listen for incoming webhooks on.")
	routesPath    = flag.String("config", "config.yml", "Path to the YAML file describing the webhook routes.")
	actionTimeout = flag.Duration("timeout", 30*time.Second, "Maximum time allowed for each forward or exec action.")
	maxBodyBytes  = flag.Int64("max-body", 10*1024*1024, "Maximum size in bytes of an accepted webhook body.")
	recordMode    = flag.String("record", "none", "Record received payloads to disk: none, unhandled or all.")
	recordDir     = flag.String("record-dir", "recordings", "Directory to write recorded payloads into.")
)

func main() {
	envflag.Parse()
	slogflags.Logger(
		slogflags.WithSetDefault(true),
		slogflags.WithOldLogLevel(slog.LevelWarn),
	)

	if err := run(); err != nil {
		slog.Error("hookmux exited with an error", "error", err)
		os.Exit(1)
	}
}

//go:embed static/landing.html
var landingPage string

//go:embed static/favicon.svg
var favicon []byte

func newHandler(routeSet *routes.Set, dispatcher *server.Dispatcher, recorder *record.Recorder, maxBody int64) http.Handler {
	webhooks := server.NewWebhook(routeSet, dispatcher, recorder, maxBody)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(landingPage))
	})
	mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(favicon)
	})
	mux.Handle("/", webhooks)

	return middleware.Chain(middleware.WithMiddleware(
		middleware.Recover(middleware.WithPanicLogger(func(r *http.Request, err any) {
			slog.Error("Recovered from panic handling request",
				"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "panic", err)
		})),
		middleware.StripTrailingSlashes(),
		middleware.TextLog(middleware.WithTextLogSink(func(line string) {
			slog.Info("request", "log", line)
		})),
	))(mux)
}

func run() error {
	routeSet, err := routes.Load(*routesPath)
	if err != nil {
		return err
	}
	slog.Info("Loaded routing configuration", "path", *routesPath, "routes", routeSet.Len())

	dispatcher := server.NewDispatcher(*actionTimeout, nil)

	var recorder *record.Recorder
	if mode, err := record.ParseMode(*recordMode); err != nil {
		return err
	} else if mode != record.ModeNone {
		recorder, err = record.New(*recordDir, mode)
		if err != nil {
			return err
		}
		slog.Info("Recording payloads", "mode", *recordMode, "directory", *recordDir)
	}
	handler := newHandler(routeSet, dispatcher, recorder, *maxBodyBytes)

	srv := &http.Server{
		Addr:              *listenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("Listening for webhooks", "address", *listenAddress, "endpoint", "/")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		slog.Info("Shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), *actionTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("Error while shutting down HTTP server", "error", err)
	}
	if err := dispatcher.Shutdown(shutdownCtx); err != nil {
		slog.Warn("Timed out waiting for in-flight actions to finish", "error", err)
	}
	slog.Info("Shutdown complete")
	return nil
}
