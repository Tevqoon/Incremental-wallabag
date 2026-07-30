// Command increader is a self-hosted incremental reader.
//
// Subcommands:
//
//	increader serve        run the web server and the background sync loop
//	increader sync         sync every configured source once, then exit
//	increader healthcheck  probe a running instance (used by the container)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Blank import of time/tzdata compiles the IANA timezone database into the
	// binary. Without it, a distroless or scratch container has no zoneinfo
	// files, time.LoadLocation fails, and every due date silently becomes UTC —
	// which for a day-scheduled reader means material appears at the wrong time.
	_ "time/tzdata"

	"github.com/Tevqoon/increader/internal/config"
	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/store"
	"github.com/Tevqoon/increader/internal/syncer"
	"github.com/Tevqoon/increader/internal/wallabag"
)

func main() {
	// main stays a thin wrapper so the real work can return errors normally
	// rather than calling os.Exit, which would skip every deferred cleanup.
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "increader:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to the configuration file")
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		command = "serve"
	}

	settings, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch command {
	case "healthcheck":
		return healthcheck(settings)
	case "sync":
		return runSync(settings, logger)
	case "serve":
		return serve(settings, logger)
	default:
		return fmt.Errorf("unknown command %q (want serve, sync or healthcheck)", command)
	}
}

// openStore opens the database and builds the configured sources.
//
// Go note: sources is a slice of the source.Source interface. *wallabag.Source
// is stored in it without any declaration linking the two types — Go checks
// structurally that the method set matches, at compile time.
func openStore(settings config.Config, logger *slog.Logger) (*store.Store, []source.Source, error) {
	db, err := store.Open(settings.Database)
	if err != nil {
		return nil, nil, err
	}

	var sources []source.Source
	if settings.Sources.Wallabag.Enabled() {
		client, err := wallabag.New(wallabag.Config{
			URL:          settings.Sources.Wallabag.URL,
			ClientID:     settings.Sources.Wallabag.ClientID,
			ClientSecret: settings.Sources.Wallabag.ClientSecret,
			Username:     settings.Sources.Wallabag.Username,
			Password:     settings.Sources.Wallabag.Password,
		})
		if err != nil {
			db.Close()
			return nil, nil, err
		}
		sources = append(sources, wallabag.NewSource(client))
	} else {
		logger.Warn("wallabag is not configured; no sources will be synced")
	}

	return db, sources, nil
}

func runSync(settings config.Config, logger *slog.Logger) error {
	db, sources, err := openStore(settings, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	// A one-shot sync should not hang forever on an unresponsive server, but
	// a first full-library sync is genuinely slow, so the budget is generous.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	results, err := syncer.New(db, logger, sources...).SyncAll(ctx)
	for _, result := range results {
		fmt.Printf("%s: %d fetched, %d new, %d updated\n",
			result.Source, result.Fetched, result.Created, result.Updated)
	}
	if err != nil {
		return err
	}

	total, err := db.CountElements("")
	if err != nil {
		return err
	}
	due, err := db.CountDue(time.Now().In(settings.Location))
	if err != nil {
		return err
	}
	fmt.Printf("queue: %d elements, %d due today\n", total, due)
	return nil
}

func serve(settings config.Config, logger *slog.Logger) error {
	db, sources, err := openStore(settings, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	// NotifyContext cancels ctx on SIGINT or SIGTERM, which is how a container
	// runtime asks a process to stop. Every long-running piece below watches it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go syncer.New(db, logger, sources...).Run(ctx, settings.SyncInterval.Duration)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.DB().PingContext(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		total, _ := db.CountElements("")
		due, _ := db.CountDue(time.Now().In(settings.Location))
		fmt.Fprintf(w, "increader\n%d elements, %d due today\n", total, due)
	})

	server := &http.Server{
		Addr:              settings.Bind,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Shutdown runs in its own goroutine because ListenAndServe blocks until
	// the server stops, so nothing after it would run otherwise.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("listening", "address", settings.Bind, "timezone", settings.Location.String())
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	logger.Info("stopped")
	return nil
}

// healthcheck probes a running instance over HTTP.
//
// It exists as a subcommand because the container image is distroless: there is
// no shell, no curl and no wget for a compose healthcheck to call, but the
// binary is already there.
func healthcheck(settings config.Config) error {
	_, port, err := net.SplitHostPort(settings.Bind)
	if err != nil {
		return fmt.Errorf("healthcheck: cannot read port from bind address %q: %w", settings.Bind, err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: server returned %s", resp.Status)
	}
	return nil
}
