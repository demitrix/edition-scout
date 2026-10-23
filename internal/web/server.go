// Package web serves edition-scout's pages and JSON API.
package web

import (
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"edition-scout/internal/hardcover"
	"edition-scout/internal/scout"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	svc     *scout.Service
	hc      *hardcover.Client
	version string
	pages   map[string]*template.Template
}

func New(svc *scout.Service, hc *hardcover.Client, version string) *Server {
	funcs := template.FuncMap{
		"date": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Local().Format("2006-01-02")
		},
		"ago":  ago,
		"join": strings.Join,
		"label": func(f string) string {
			if f == scout.FormatAudio {
				return "Audiobook"
			}
			return "Ebook"
		},
		"sourceName": sourceName,
		"first": func(eds []scout.Edition) *scout.Edition {
			if len(eds) == 0 {
				return nil
			}
			return &eds[0]
		},
	}
	s := &Server{svc: svc, hc: hc, version: version, pages: map[string]*template.Template{}}
	for _, p := range []string{"home", "search", "author", "building", "report", "error"} {
		s.pages[p] = template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+p+".html"))
	}
	return s
}

func sourceName(s string) string {
	switch s {
	case "googlebooks":
		return "Google Books"
	case "openlibrary":
		return "Open Library"
	case "audible":
		return "Audible"
	case "goodreads":
		return "Goodreads"
	case "hardcover":
		return "Hardcover"
	}
	return s
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h ago"
	}
	return strconv.Itoa(int(d.Hours()/24)) + " days ago"
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /search", s.search)
	mux.HandleFunc("GET /author/{id}", s.author)
	mux.HandleFunc("POST /author/{id}/refresh", s.refresh)
	mux.HandleFunc("POST /author/{id}/remove", s.remove)
	mux.HandleFunc("GET /report", s.report)

	mux.HandleFunc("GET /api/search", s.apiSearch)
	mux.HandleFunc("GET /api/authors", s.apiAuthors)
	mux.HandleFunc("GET /api/authors/{id}", s.apiAuthor)
	mux.HandleFunc("GET /api/report", s.apiReport)
	mux.HandleFunc("GET /api/status", s.apiStatus)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	return logRequests(mux)
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			slog.Debug("request", "method", r.Method, "path", r.URL.Path, "ms", time.Since(start).Milliseconds())
		}
	})
}

// ---- Pages ----

type page struct {
	Title   string
	Version string
	Status  status
	Query   string
	Data    any
}

type status struct {
	Configured bool                 `json:"hardcoverConfigured"`
	Pending    map[string]int       `json:"pendingLookups"`
	Paused     map[string]time.Time `json:"pausedSources"`
	Cached     int                  `json:"cachedResults"`
	Hardcover  hardcover.Stats      `json:"hardcover"`
	Sources    []string             `json:"sources"`
}

func (s *Server) status() status {
	q := s.svc.Gaps.Queue()
	return status{Configured: s.hc.Configured(), Pending: q.Pending, Paused: q.Paused, Cached: q.Cached,
		Hardcover: s.hc.Stats(), Sources: s.svc.Gaps.SourceNames()}
}

func (s *Server) render(w http.ResponseWriter, name string, code int, p page) {
	p.Version, p.Status = s.version, s.status()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := s.pages[name].Execute(w, p); err != nil {
		slog.Error("template", "page", name, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, code int, msg string) {
	s.render(w, "error", code, page{Title: http.StatusText(code), Data: msg})
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	s.render(w, "home", http.StatusOK, page{Title: "Authors", Data: s.svc.Summaries()})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// A numeric Hardcover author id goes straight to the author.
	if id := authorIDFromQuery(q); id > 0 {
		http.Redirect(w, r, "/author/"+strconv.Itoa(id), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	hits, err := s.svc.Catalog.SearchAuthors(ctx, q)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "Hardcover search failed: "+err.Error())
		return
	}
	s.render(w, "search", http.StatusOK, page{Title: "Search", Query: q, Data: hits})
}

func authorIDFromQuery(q string) int {
	if n, err := strconv.Atoi(q); err == nil && n > 0 {
		return n
	}
	return 0
}

type authorPage struct {
	Author   *scout.Author
	Books    []scout.BookView
	Counts   scout.Counts
	Building bool
	OnlyGaps bool
	Enabled  map[string]bool
}

func (s *Server) author(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		s.fail(w, http.StatusNotFound, "No such author.")
		return
	}
	s.svc.Track(id, "")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	a, err := s.svc.Author(ctx, id, false)
	switch {
	case errors.Is(err, scout.ErrNotFound):
		s.svc.Untrack(id)
		s.fail(w, http.StatusNotFound, "Hardcover has no such author (placeholder records like “Various” aren't shown).")
		return
	case errors.Is(err, scout.ErrBuilding) && a == nil:
		s.render(w, "building", http.StatusOK, page{Title: "Loading author"})
		return
	case err != nil && !errors.Is(err, scout.ErrBuilding):
		if a == nil {
			s.fail(w, http.StatusBadGateway, "Reading the author from Hardcover failed: "+err.Error())
			return
		}
	}
	books, counts := s.svc.View(a)
	only := r.URL.Query().Get("show") == "gaps"
	if only {
		var keep []scout.BookView
		for _, b := range books {
			if b.Ebook.State != scout.StateOnHardcover || b.Audio.State != scout.StateOnHardcover {
				keep = append(keep, b)
			}
		}
		books = keep
	}
	s.render(w, "author", http.StatusOK, page{Title: a.Name, Data: authorPage{
		Author: a, Books: books, Counts: counts, Building: s.svc.Building(id), OnlyGaps: only,
		Enabled: map[string]bool{scout.FormatEbook: s.svc.Gaps.Enabled(scout.FormatEbook), scout.FormatAudio: s.svc.Gaps.Enabled(scout.FormatAudio)},
	}})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	_, _ = s.svc.Author(ctx, id, true) // starts the rebuild; the page shows progress
	http.Redirect(w, r, "/author/"+strconv.Itoa(id), http.StatusSeeOther)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.svc.Untrack(id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type reportPage struct {
	Rows     []scout.ReportRow
	Resolved []scout.Resolved
	Ebooks   int
	Audio    int
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	rows, resolved := s.svc.Report()
	if r.URL.Query().Get("format") == "csv" {
		writeCSV(w, rows)
		return
	}
	p := reportPage{Rows: rows, Resolved: resolved}
	for _, row := range rows {
		if row.Format == scout.FormatAudio {
			p.Audio++
		} else {
			p.Ebooks++
		}
	}
	s.render(w, "report", http.StatusOK, page{Title: "Missing on Hardcover", Data: p})
}

func writeCSV(w http.ResponseWriter, rows []scout.ReportRow) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="missing-on-hardcover.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"author", "title", "format", "hardcover_url", "found_on", "edition_title", "edition_format",
		"isbn13", "isbn10", "asin", "publisher", "date", "narrators", "duration_min", "pages", "source_url"})
	for _, r := range rows {
		for _, e := range r.Editions {
			_ = cw.Write([]string{r.AuthorName, r.Title, r.Format, r.HardcoverURL, sourceName(r.Source), e.Title, e.EditionFormat,
				e.ISBN13, e.ISBN10, e.ASIN, e.Publisher, e.Date, strings.Join(e.Narrators, "; "),
				itoa(e.DurationMin), itoa(e.Pages), e.URL})
		}
	}
	cw.Flush()
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// ---- JSON API ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	hits, err := s.svc.Catalog.SearchAuthors(ctx, r.URL.Query().Get("q"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if hits == nil {
		hits = []scout.AuthorHit{}
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) apiAuthors(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Summaries())
}

// GET /api/authors/{id}: builds (and tracks) the author like the page does.
// While a first build is still running the answer is 202 with Retry-After.
func (s *Server) apiAuthor(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such author"})
		return
	}
	s.svc.Track(id, "")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	a, err := s.svc.Author(ctx, id, r.URL.Query().Get("refresh") == "1")
	switch {
	case errors.Is(err, scout.ErrNotFound):
		s.svc.Untrack(id)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such author"})
		return
	case a == nil && errors.Is(err, scout.ErrBuilding):
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "building"})
		return
	case a == nil:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	books, counts := s.svc.View(a)
	writeJSON(w, http.StatusOK, map[string]any{
		"author": map[string]any{"id": a.ID, "name": a.Name, "slug": a.Slug, "hardcoverUrl": a.URL(), "built": a.Built},
		"counts": counts, "building": s.svc.Building(id), "books": books,
	})
}

func (s *Server) apiReport(w http.ResponseWriter, _ *http.Request) {
	rows, resolved := s.svc.Report()
	if rows == nil {
		rows = []scout.ReportRow{}
	}
	if resolved == nil {
		resolved = []scout.Resolved{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"missingOnHardcover": rows, "resolvedRecently": resolved})
}

func (s *Server) apiStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.status())
}
