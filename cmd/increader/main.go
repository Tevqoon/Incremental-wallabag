// Command increader is a self-hosted incremental reader.
//
// Subcommands:
//
//	increader serve             run the web server and the background sync loop
//	increader sync              sync every configured source once, then exit
//	increader healthcheck       probe a running instance (used by the container)
//	increader import-substack   backfill a Substack archive into wallabag in place
//	increader version           print the build version and exit
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
	"strings"
	"syscall"
	"time"

	// Blank import of time/tzdata compiles the IANA timezone database into the
	// binary. Without it, a distroless or scratch container has no zoneinfo
	// files, time.LoadLocation fails, and every due date silently becomes UTC —
	// which for a day-scheduled reader means material appears at the wrong time.
	_ "time/tzdata"

	"github.com/Tevqoon/increader/internal/config"
	"github.com/Tevqoon/increader/internal/ingest"
	"github.com/Tevqoon/increader/internal/proofread"
	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/store"
	"github.com/Tevqoon/increader/internal/substack"
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
	"serve": true, "sync": true, "healthcheck": true, "version": true, "import-substack": true,
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
	commit := flags.Bool("commit", false, "import-substack: apply changes instead of only reporting them")
	host := flags.String("host", "", "import-substack: override the configured publication host")
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
	case "import-substack":
		return importSubstack(settings, logger, *commit, *host)
	default:
		return fmt.Errorf("unknown command %q (want serve, sync, healthcheck, import-substack or version)", command)
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
		WithAnnotationDelay(settings.AnnotationDelayDays, settings.AnnotationDelaySpreadDays).
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

// importSubstack backfills one Substack publication's archive into wallabag
// in place — replacing paywall previews with full posts on the same entry,
// re-anchoring any highlights already made on it. importSubstack is only
// this command's own wiring (flags, the database it opens, a generous
// context, printing the result); refreshSubstackArchive below is the actual
// work, shared with the import page's own "check for new articles" button.
func importSubstack(settings config.Config, logger *slog.Logger, commit bool, hostOverride string) error {
	if hostOverride != "" {
		settings.Ingest.Substack.Host = hostOverride
	}

	db, err := store.Open(settings.Database)
	if err != nil {
		return fmt.Errorf("import-substack: %w", err)
	}
	defer db.Close()

	// A full archive walk runs at roughly 1.5s per post (RequestGap, kept
	// deliberately polite toward a service with no documented rate limit)
	// over a few hundred posts for an established publication, plus the
	// wallabag reconcile pass afterward — comfortably past runSync's own
	// 30-minute budget, which only ever covers a single incremental sync.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	report, err := refreshSubstackArchive(ctx, db, settings, logger, commit)
	if report != "" {
		fmt.Print(report)
	}
	if err != nil {
		return fmt.Errorf("import-substack: %w", err)
	}
	return nil
}

// refreshSubstackFeedHandler returns the closure serve wires into
// web.Options.RefreshSubstackFeed, or nil when ingest.substack is not
// configured — nil is what hides the import page's "check for new
// articles" button entirely, the same on/off switch
// importSubstackURLHandler uses for the single-URL section right above it.
func refreshSubstackFeedHandler(db *store.Store, settings config.Config, logger *slog.Logger) func(context.Context) (string, error) {
	if !settings.Ingest.Substack.Enabled() {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		// Always commits: unlike the CLI's own default, this button is
		// reached from a click on a page whose only reason to exist is
		// bringing wallabag up to date, and it can only ever be run again —
		// there is no separate "review, then apply" step for a human to
		// skip past by mistake the way there would be for a script run
		// unattended. See refreshSubstackArchive's own doc comment for the
		// commit-order guarantee that makes committing unconditionally here
		// safe regardless.
		return refreshSubstackArchive(ctx, db, settings, logger, true)
	}
}

// refreshSubstackArchive walks one publication's whole archive and
// reconciles what it finds into wallabag in place — the shared core both
// the import-substack command and the import page's own web button call.
// Returns the same operator-facing report either would otherwise print
// straight to a terminal, as a string instead, so a caller can show it
// however fits (os.Stdout for the CLI, an HTML <pre> for the web button)
// without this function knowing or caring which.
//
// The plan is always built and reported before anything is written,
// whether or not commit is set — Gather, BuildPlan and the first
// WriteReport all run unconditionally. That ordering is deliberate, not an
// optimisation left for later: it makes it structurally impossible for
// commit to apply a plan that was never reported first, because there is
// only one place BuildPlan is called and Apply always runs after it, never
// instead of it.
func refreshSubstackArchive(ctx context.Context, db *store.Store, settings config.Config, logger *slog.Logger, commit bool) (string, error) {
	if !settings.Ingest.Substack.Enabled() {
		return "", fmt.Errorf("ingest.substack is not configured (need host and session_cookie); " +
			"see config.yaml's commented-out ingest.substack block")
	}
	if !settings.Sources.Wallabag.Enabled() {
		return "", fmt.Errorf("sources.wallabag is not configured; this reconciles into wallabag and cannot do that without it")
	}

	var out strings.Builder
	if commit {
		fmt.Fprintln(&out, "=== APPLYING — changes will be written to wallabag and the local store ===")
	} else {
		fmt.Fprintln(&out, "=== DRY RUN — reporting only, nothing will be written (pass -commit to apply) ===")
	}

	// openStore builds a *wallabag.Client internally and discards it once
	// it has wrapped it in a source.Source, because every other caller only
	// ever needs the Source. This needs the raw Client too — Gather and
	// Apply both take one directly — so it is built again here rather than
	// widening openStore's return signature for the sake of this one
	// caller.
	client, err := wallabag.New(wallabag.Config{
		URL:          settings.Sources.Wallabag.URL,
		ClientID:     settings.Sources.Wallabag.ClientID,
		ClientSecret: settings.Sources.Wallabag.ClientSecret,
		Username:     settings.Sources.Wallabag.Username,
		Password:     settings.Sources.Wallabag.Password,
	})
	if err != nil {
		return "", err
	}
	src := wallabag.NewSource(client)

	imp, err := substack.New(substack.Config{
		Host:      settings.Ingest.Substack.Host,
		SessionID: settings.Ingest.Substack.SessionCookie,
		CacheDir:  settings.Ingest.Substack.CacheDir,
	})
	if err != nil {
		return "", err
	}

	docs, result, err := imp.Ingest(ctx, logger)
	if err != nil {
		return "", fmt.Errorf("fetch archive: %w", err)
	}
	fmt.Fprintf(&out, "substack: %d archive pages, %d posts found (%d cached, %d fetched, %d skipped as non-newsletter)\n",
		result.Pages, result.Posts, result.Cached, result.Fetched, result.SkippedNonNewsletter)
	for _, warning := range result.Warnings {
		fmt.Fprintln(&out, "substack warning:", warning)
	}

	// Appended, not replaced: a post can already carry tags of its own (a
	// section name Substack itself assigns, say), and this importer's own
	// tag is only meant to make a backfilled entry easy to find afterward,
	// not to erase whatever else already described it.
	for i := range docs {
		docs[i].Tags = append(docs[i].Tags, settings.Ingest.Substack.Tag)
	}

	snap, err := ingest.Gather(ctx, client, docs)
	if err != nil {
		return "", err
	}
	now := time.Now()
	plan := ingest.BuildPlan(docs, snap, now)
	if err := ingest.WriteReport(&out, plan, nil); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}

	if !commit {
		fmt.Fprintln(&out, "=== dry run complete — nothing was written; re-run with -commit to apply the plan above ===")
		return out.String(), nil
	}

	applied, err := ingest.Apply(ctx, client, src, plan, logger)
	if err != nil {
		// Apply itself only returns an error for something that aborted the
		// whole batch (a cancelled context); a single bad entry or
		// annotation is recorded in applied.Errors / AnnotationFailures
		// instead and must not fail this call, since it would still leave
		// every entry applied before the failure correctly written.
		return "", fmt.Errorf("apply: %w", err)
	}

	repaired, err := ingest.Repair(ctx, db, applied, now, logger)
	if err != nil {
		return "", fmt.Errorf("repair: %w", err)
	}

	if err := ingest.WriteReport(&out, plan, &applied); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintf(&out, "repair: %d entries repaired locally, %d skipped (no local row yet), %d errors\n",
		repaired.Repaired, repaired.Skipped, len(repaired.Errors))
	for _, repairErr := range repaired.Errors {
		fmt.Fprintln(&out, "repair warning:", repairErr)
	}
	fmt.Fprintln(&out, "=== apply complete — changes were written to wallabag and the local store ===")

	return out.String(), nil
}

// importSubstackURLHandler returns the closure serve wires into
// web.Options.ImportSubstackURL, or nil when there is no session cookie
// configured to fetch with — nil is what hides the import page's "from a
// URL" section entirely, the same on/off switch Enabled() is for the
// archive-backfill command, just without requiring Host too (see
// importSubstackURL's own doc comment for why Host does not belong in that
// check here).
func importSubstackURLHandler(db *store.Store, settings config.Config, logger *slog.Logger) func(context.Context, string) (string, error) {
	if settings.Ingest.Substack.SessionCookie == "" {
		return nil
	}
	return func(ctx context.Context, rawURL string) (string, error) {
		return importSubstackURL(ctx, db, settings, logger, rawURL)
	}
}

// importSubstackURL fetches and reconciles exactly one Substack post into
// wallabag — the web-triggered counterpart to importSubstack, scoped to a
// single URL instead of a whole archive. This is what backs the import
// page's "from a URL" section (see web.Options.ImportSubstackURL), for the
// common case importSubstack's own standing archive-backfill configuration
// does not fit well: one or two articles from a publication the operator
// has no ongoing reason to configure a whole backfill for, pulled in
// because wallabag's own Substack extraction drops structure — headings,
// most visibly — that internal/substack's own cleaning keeps.
//
// Unlike importSubstack, the publication host comes from the URL itself,
// not settings.Ingest.Substack.Host: that field (and Enabled()) describes
// the one publication a standing archive backfill is configured for, which
// has nothing to do with which publication a pasted URL happens to belong
// to. Only the session cookie — the operator's own account, valid across
// any publication it can reach — and wallabag being configured are
// required here. db is the already-open store the caller (serve) holds,
// reused rather than opened again; client and src are still built fresh
// here, the same as importSubstack already does and for the same reason
// (see its own comment on openStore): internal/ingest needs the concrete
// *wallabag.Client and *wallabag.Source, which openStore does not expose.
func importSubstackURL(ctx context.Context, db *store.Store, settings config.Config, logger *slog.Logger, rawURL string) (string, error) {
	if settings.Ingest.Substack.SessionCookie == "" {
		return "", fmt.Errorf("substack import is not configured (need ingest.substack.session_cookie in config.yaml)")
	}
	if !settings.Sources.Wallabag.Enabled() {
		return "", fmt.Errorf("substack import needs sources.wallabag configured; this reconciles into wallabag")
	}

	host, slug, err := substack.PostFromURL(rawURL)
	if err != nil {
		return "", err
	}

	cacheDir := settings.Ingest.Substack.CacheDir
	if cacheDir == "" {
		// Load only fills this in when Ingest.Substack.Host is also set (see
		// config.Load) — a reasonable gap for the archive-backfill path,
		// where Host is required anyway, but this path needs a value even
		// when Host was never configured at all, since it never reads Host
		// in the first place.
		cacheDir = "./substack-cache"
	}

	imp, err := substack.New(substack.Config{
		Host:      host,
		SessionID: settings.Ingest.Substack.SessionCookie,
		CacheDir:  cacheDir,
	})
	if err != nil {
		return "", err
	}

	doc, warnings, err := imp.FetchPost(ctx, slug)
	if err != nil {
		return "", err
	}
	// Tagged with the post's own publication, not settings.Ingest.Substack.Tag
	// — that default names the one publication the archive backfill is
	// configured for, which this post very often is not.
	doc.Tags = append(doc.Tags, strings.SplitN(host, ".", 2)[0])
	docs := []source.Document{doc}

	client, err := wallabag.New(wallabag.Config{
		URL:          settings.Sources.Wallabag.URL,
		ClientID:     settings.Sources.Wallabag.ClientID,
		ClientSecret: settings.Sources.Wallabag.ClientSecret,
		Username:     settings.Sources.Wallabag.Username,
		Password:     settings.Sources.Wallabag.Password,
	})
	if err != nil {
		return "", err
	}
	src := wallabag.NewSource(client)

	snap, err := ingest.Gather(ctx, client, docs)
	if err != nil {
		return "", err
	}
	now := time.Now()
	plan := ingest.BuildPlan(docs, snap, now)

	applied, err := ingest.Apply(ctx, client, src, plan, logger)
	if err != nil {
		return "", err
	}
	repaired, err := ingest.Repair(ctx, db, applied, now, logger)
	if err != nil {
		return "", err
	}

	var report strings.Builder
	for _, warning := range warnings {
		fmt.Fprintln(&report, "substack warning:", warning)
	}
	if err := ingest.WriteReport(&report, plan, &applied); err != nil {
		return "", err
	}
	fmt.Fprintf(&report, "repair: %d entries repaired locally, %d skipped (no local row yet), %d errors\n",
		repaired.Repaired, repaired.Skipped, len(repaired.Errors))
	for _, repairErr := range repaired.Errors {
		fmt.Fprintln(&report, "repair warning:", repairErr)
	}
	return report.String(), nil
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

	sync := syncer.New(db, logger, sources...).
		WithAnnotationDelay(settings.AnnotationDelayDays, settings.AnnotationDelaySpreadDays)
	go sync.Run(ctx, settings.SyncInterval.Duration)

	// The reader looks documents up by their source name when it needs to fetch
	// an article body on first open.
	byName := make(map[string]source.Source, len(sources))
	for _, provider := range sources {
		byName[provider.Name()] = provider
	}

	// nil, not a Client with an empty key, when unconfigured: web.Server
	// checks this pointer directly to decide whether to offer the "Fix
	// typos" action at all, the same nil-hides-the-feature convention
	// ImportSubstackURL and RefreshSubstackFeed already use below.
	var proofreader *proofread.Client
	if settings.LLM.Enabled() {
		proofreader = proofread.NewClient(settings.LLM.APIKey, settings.LLM.BaseURL, settings.LLM.Model)
	}

	reader, err := web.New(web.Options{
		Store:                     db,
		Sources:                   byName,
		QueuePageLimit:            settings.QueuePageLimit,
		ExtractDelay:              settings.ExtractDelayDays,
		AnnotationDelayDays:       settings.AnnotationDelayDays,
		AnnotationDelaySpreadDays: settings.AnnotationDelaySpreadDays,
		Logger:                    logger,
		Publish:                   sync.Publish,
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
		ImportSubstackURL:   importSubstackURLHandler(db, settings, logger),
		RefreshSubstackFeed: refreshSubstackFeedHandler(db, settings, logger),
		Proofreader:         proofreader,
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
// It exists as a subcommand so compose's healthcheck can call the binary
// that's already there rather than depend on curl or wget being installed
// in the image — true necessity back when the base image was distroless,
// and kept on Alpine so the healthcheck doesn't care which base is in use.
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
