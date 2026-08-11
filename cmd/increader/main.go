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
	"github.com/Tevqoon/increader/internal/version"
	"github.com/Tevqoon/increader/internal/wallabag"
	"github.com/Tevqoon/increader/internal/web"
)

func main() {
	// main stays a thin wrapper so the real work can return errors normally
	// rather than calling os.Exit, which would skip every deferred cleanup.
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "increader:", err)
		os.Exit(1)
	}
}

// commands are the subcommands increader accepts.
var commands = map[string]bool{
	"serve": true, "sync": true, "healthcheck": true, "version": true,
}

// splitCommand separates a leading subcommand from the flags.
//
// The stdlib flag package stops parsing at the first non-flag argument, so
// `increader serve -config /config.yaml` would silently ignore -config and fall
// back to the default path — the failure mode being a confusing "no such file"
// naming a file the caller never asked for. Pulling a leading subcommand off
// first makes both orderings work.
//
// It only treats the *first* argument as a command, so a flag value that
// happens to be spelled like one (-config serve) is never mistaken for it.
func splitCommand(args []string) (command string, rest []string) {
	if len(args) > 0 && commands[args[0]] {
		return args[0], args[1:]
	}
	return "", args
}

func run() error {
	command, args := splitCommand(os.Args[1:])

	flags := flag.NewFlagSet("increader", flag.ExitOnError)
	configPath := flags.String("config", "config.yaml", "path to the configuration file")
	full := flags.Bool("full", false, "sync: ignore the watermark and re-read every entry")
	if err := flags.Parse(args); err != nil {
		return err
	}

	// Flags-first form: `increader -config /config.yaml sync`.
	if command == "" {
		command = flags.Arg(0)
	}
	if command == "" {
		command = "serve"
	}

	// Answered before the config is read, so it works on a broken or missing
	// one — which is exactly when you most want to know what is running.
	if command == "version" {
		fmt.Println(version.Current().Short())
		return nil
	}

	settings, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Pin the process's local timezone to the configured one, so that every
	// date — in the scheduler, in SQL, in the templates — agrees on when today
	// began without each of them having to be handed a *time.Location.
	// Assigning time.Local is legitimate for an application (as opposed to a
	// library) and must happen before anything computes a date.
	time.Local = settings.Location

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch command {
	case "healthcheck":
		return healthcheck(settings)
	case "sync":
		return runSync(settings, logger, *full)
	case "serve":
		return serve(settings, logger)
	default:
		return fmt.Errorf("unknown command %q (want serve, sync, healthcheck or version)", command)
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

func runSync(settings config.Config, logger *slog.Logger, full bool) error {
	db, sources, err := openStore(settings, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	// A full sync is the repair path: it re-reads state that only arrives with
	// a listing — archive flags, annotations — for entries that have not
	// changed since the last run and would otherwise be skipped entirely.
	if full {
		for _, provider := range sources {
			if err := db.ResetWatermark(provider.Name()); err != nil {
				return err
			}
		}
		logger.Info("watermark cleared; re-reading every entry")
	}

	// A one-shot sync should not hang forever on an unresponsive server, but
	// a first full-library sync is genuinely slow, so the budget is generous.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	results, err := syncer.New(db, logger, sources...).
		WithExtractDelay(settings.ExtractDelayDays).
		SyncAll(ctx)
	for _, result := range results {
		fmt.Printf("%s: %d fetched, %d new, %d updated, %d archived, %d highlights\n",
			result.Source, result.Fetched, result.Created, result.Updated,
			result.Suspended, result.Highlights)
	}
	if err != nil {
		return err
	}

	total, err := db.CountElements("")
	if err != nil {
		return err
	}
	today := time.Now().In(settings.Location)
	articles, err := db.CountDue(today, store.QueueArticles)
	if err != nil {
		return err
	}
	extracts, err := db.CountDue(today, store.QueueExtracts)
	if err != nil {
		return err
	}
	fmt.Printf("queue: %d elements, %d articles and %d extracts due today\n",
		total, articles, extracts)
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

	sync := syncer.New(db, logger, sources...).WithExtractDelay(settings.ExtractDelayDays)
	go sync.Run(ctx, settings.SyncInterval.Duration)

	// The reader looks documents up by their source name when it needs to fetch
	// an article body on first open.
	byName := make(map[string]source.Source, len(sources))
	for _, provider := range sources {
		byName[provider.Name()] = provider
	}

	reader, err := web.New(web.Options{
		Store:        db,
		Sources:      byName,
		DailyLimit:   settings.DailyLimit,
		ExtractDelay: settings.ExtractDelayDays,
		Logger:       logger,
		Publish:      sync.Publish,
		SyncNow: func(ctx context.Context) error {
			// Reconciling here too, rather than waiting for the scheduled
			// loop's daily check: a manual sync is exactly the moment
			// someone wants to know the library actually matches wallabag,
			// not just that anything new has arrived.
			_, syncErr := sync.SyncAll(ctx)
			reconcileErr := sync.Reconcile(ctx)
			if syncErr != nil {
				return syncErr
			}
			return reconcileErr
		},
	})
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              settings.Bind,
		Handler:           reader.Handler(),
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

	logger.Info("listening",
		"address", settings.Bind,
		"timezone", settings.Location.String(),
		"build", version.Current().Short())
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
