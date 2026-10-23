// Command edition-scout serves a web app that shows an author's Hardcover
// catalogue and finds the ebook and audiobook editions Hardcover is missing.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"edition-scout/internal/hardcover"
	"edition-scout/internal/scout"
	"edition-scout/internal/web"
)

var version = "dev"

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil {
		return n
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(env(key, "")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func main() {
	level := slog.LevelInfo
	if envBool("SCOUT_DEBUG", false) {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	lang, ok := scout.LookupLanguage(env("SCOUT_LANGUAGE", "en"))
	if !ok {
		slog.Error("unsupported SCOUT_LANGUAGE (use en, de, fr, es, it, nl or pt)")
		os.Exit(2)
	}
	dataDir := env("SCOUT_DATA_DIR", "/data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		slog.Error("data directory not writable", "dir", dataDir, "err", err)
		os.Exit(2)
	}
	maxAge, err := time.ParseDuration(env("SCOUT_REFRESH_INTERVAL", "24h"))
	if err != nil || maxAge < time.Hour {
		maxAge = 24 * time.Hour
	}

	hc := hardcover.New(os.Getenv("HARDCOVER_API_TOKEN"), "edition-scout/"+version)
	if !hc.Configured() {
		slog.Warn("HARDCOVER_API_TOKEN is not set; nothing can be looked up until it is")
	}
	catalog := &scout.Catalog{HC: hc, Lang: lang,
		IncludeCompilations: envBool("SCOUT_INCLUDE_COMPILATIONS", true),
		EditionsPerFormat:   envInt("SCOUT_EDITIONS_PER_FORMAT", 3)}

	// Sources, in lookup order per format.
	var sources []scout.Source
	goodreads := envBool("SCOUT_GOODREADS", false)
	if goodreads {
		sources = append(sources, scout.NewGoodreads(lang))
	}
	if region := env("SCOUT_AUDIBLE_REGION", "us"); !strings.EqualFold(region, "off") {
		a, err := scout.NewAudible(region, lang)
		if err != nil {
			slog.Error("SCOUT_AUDIBLE_REGION", "err", err)
			os.Exit(2)
		}
		// Audible before Goodreads for audiobooks: richer data, real API.
		sources = append([]scout.Source{a}, sources...)
	}
	if key := os.Getenv("GOOGLE_BOOKS_API_KEY"); key != "" {
		sources = append(sources, scout.NewGoogleBooks(key, lang))
	}
	if envBool("SCOUT_OPENLIBRARY", true) {
		sources = append(sources, scout.NewOpenLibrary(lang))
	}

	gaps := scout.NewGaps(sources, dataDir)
	svc := scout.NewService(catalog, gaps, dataDir, maxAge, envInt("SCOUT_PRINT_ONLY_MIN_READERS", 5))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.Refresh(ctx)

	srv := &http.Server{Addr: env("SCOUT_ADDR", ":8080"), Handler: web.New(svc, hc, version).Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		slog.Info("edition-scout listening", "addr", srv.Addr, "version", version, "language", lang.Code2,
			"goodreads", goodreads, "dataDir", dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	gaps.Flush()
}
