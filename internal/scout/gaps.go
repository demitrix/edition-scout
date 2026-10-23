package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Gaps finds editions Hardcover lacks. For each book with no ebook or no
// audiobook edition on Hardcover, a background lookup asks the enabled
// sources in order and records what it finds (or that nothing exists).
//
// Lookups never run on a page request: sources rate-limit (Google Books
// allows 1,000 requests a day) and answer with transient errors. Results are
// cached on disk, so restarts don't repeat them, and a source that's paused
// (quota used up, bot check) holds its lookups until it's available again
// instead of recording a miss it might not have.

const (
	gapHitTTL     = 30 * 24 * time.Hour
	gapMissTTL    = 14 * 24 * time.Hour
	gapRetryDelay = 10 * time.Minute
	gapMaxRetries = 3
	gapQueueLimit = 20000
	resolvedKeep  = 30 * 24 * time.Hour
)

// GapEntry is the result of looking for one format of one book elsewhere.
type GapEntry struct {
	BookID     int       `json:"bookId"`
	Format     string    `json:"format"`
	Title      string    `json:"title"`
	Slug       string    `json:"slug,omitempty"`
	AuthorID   int       `json:"authorId"`
	AuthorName string    `json:"authorName"`
	Editions   []Edition `json:"editions"`
	Source     string    `json:"source,omitempty"` // source of Editions
	Tried      []string  `json:"tried"`            // sources that answered
	Checked    time.Time `json:"checked"`
	FoundAt    time.Time `json:"foundAt,omitempty"`
	Expires    time.Time `json:"expires"`
}

func (e GapEntry) Found() bool { return len(e.Editions) > 0 }

// Resolved records a gap that Hardcover has since filled.
type Resolved struct {
	BookID     int       `json:"bookId"`
	Format     string    `json:"format"`
	Title      string    `json:"title"`
	Slug       string    `json:"slug,omitempty"`
	AuthorID   int       `json:"authorId"`
	AuthorName string    `json:"authorName"`
	WasFound   bool      `json:"wasFound"`
	ResolvedAt time.Time `json:"resolvedAt"`
}

type gapJob struct {
	BookID   int    `json:"bookId"`
	Format   string `json:"format"`
	Title    string `json:"title"`
	Slug     string `json:"slug,omitempty"`
	AuthorID int    `json:"authorId"`
	Author   string `json:"author"`
	Attempt  int    `json:"attempt,omitempty"`
}

func gapKey(bookID int, format string) string { return fmt.Sprintf("%d/%s", bookID, format) }

func (j gapJob) key() string { return gapKey(j.BookID, j.Format) }

type Gaps struct {
	sources []Source
	dir     string

	mu       sync.Mutex
	entries  map[string]GapEntry
	pending  map[string]gapJob
	resolved map[string]Resolved
	paused   map[string]time.Time // source name -> until
	skipped  map[string]string    // gap key -> why it isn't looked up (from the last build)
	dirty    bool
	queues   map[string]chan gapJob
}

type gapState struct {
	Paused map[string]time.Time `json:"paused"`
	Jobs   []gapJob             `json:"jobs"`
}

func NewGaps(sources []Source, dir string) *Gaps {
	g := &Gaps{sources: sources, dir: dir, entries: map[string]GapEntry{}, pending: map[string]gapJob{},
		resolved: map[string]Resolved{}, paused: map[string]time.Time{}, skipped: map[string]string{},
		queues: map[string]chan gapJob{}}
	for _, f := range GapFormats {
		g.queues[f] = make(chan gapJob, gapQueueLimit)
	}
	if dir != "" {
		readJSON(filepath.Join(dir, "gaps.json"), &g.entries)
		readJSON(filepath.Join(dir, "gaps-resolved.json"), &g.resolved)
		var st gapState
		readJSON(filepath.Join(dir, "gaps-queue.json"), &st)
		for k, v := range st.Paused {
			g.paused[k] = v
		}
		for _, j := range st.Jobs {
			if q, ok := g.queues[j.Format]; ok && len(g.sourcesFor(j.Format)) > 0 {
				select {
				case q <- j:
					g.pending[j.key()] = j
				default:
				}
			}
		}
		go func() {
			for range time.Tick(time.Minute) {
				g.Flush()
			}
		}()
	}
	for f, q := range g.queues {
		go g.worker(f, q)
	}
	names := []string{}
	for _, s := range sources {
		names = append(names, s.Name())
	}
	slog.Info("edition lookups", "sources", strings.Join(names, ","), "cached", len(g.entries), "queued", len(g.pending))
	return g
}

func (g *Gaps) sourcesFor(format string) []Source {
	var out []Source
	for _, s := range g.sources {
		if s.Supports(format) {
			out = append(out, s)
		}
	}
	return out
}

// SourceNames lists the enabled sources in lookup order.
func (g *Gaps) SourceNames() []string {
	out := make([]string, 0, len(g.sources))
	for _, s := range g.sources {
		out = append(out, s.Name())
	}
	return out
}

// Enabled reports whether any source can look up format.
func (g *Gaps) Enabled(format string) bool { return len(g.sourcesFor(format)) > 0 }

// Process checks an author's freshly built catalogue: gaps Hardcover has
// filled are resolved, and gaps without a current result are queued.
// minReaders: books with neither an ebook nor an audiobook on Hardcover
// (print-only) are only looked up from this many Hardcover readers up.
func (g *Gaps) Process(a *Author, minReaders int) (queued int) {
	// A book whose title contains another book's title that already has the
	// format is a duplicate record or a split part ("The Way of Kings, Part
	// 2"), not a missing edition.
	with := map[string][]string{}
	for _, b := range a.Books {
		if t := normalize(b.Title); len(t) >= 6 {
			for _, f := range GapFormats {
				if b.Has(f) {
					with[f] = append(with[f], t)
				}
			}
		}
	}
	partOf := func(title, format string) bool {
		t := normalize(title)
		for _, w := range with[format] {
			if t != w && strings.Contains(t, w) {
				return true
			}
		}
		return false
	}

	for _, b := range a.Books {
		for _, f := range GapFormats {
			key := gapKey(b.ID, f)
			if b.Has(f) {
				g.resolve(key, b, a)
				continue
			}
			printOnly := !b.Has(FormatEbook) && !b.Has(FormatAudio)
			reason := ""
			switch {
			case !g.Enabled(f):
				reason = "no source enabled for this format"
			case placeholderTitle(b.Title):
				reason = "unannounced title"
			case partOf(b.Title, f):
				reason = "part or duplicate of another book that has it"
			case printOnly && b.Readers < minReaders:
				reason = "print only, few Hardcover readers"
			}
			g.mu.Lock()
			if reason != "" {
				g.skipped[key] = reason
			} else {
				delete(g.skipped, key)
			}
			g.mu.Unlock()
			if reason != "" {
				g.forget(key)
				continue
			}
			if _, ok := g.current(key); ok {
				continue
			}
			job := gapJob{BookID: b.ID, Format: f, Title: b.Title, Slug: b.Slug, AuthorID: a.ID, Author: a.Name}
			g.mu.Lock()
			if _, already := g.pending[key]; !already {
				select {
				case g.queues[f] <- job:
					g.pending[key] = job
					queued++
				default: // queue full; the next refresh retries
				}
			}
			g.mu.Unlock()
		}
	}
	return queued
}

// current returns a cached result that's still valid: not expired, and (for
// a miss) asked of every source now enabled for the format.
func (g *Gaps) current(key string) (GapEntry, bool) {
	g.mu.Lock()
	e, ok := g.entries[key]
	g.mu.Unlock()
	if !ok || time.Now().After(e.Expires) {
		return GapEntry{}, false
	}
	if !e.Found() {
		for _, s := range g.sourcesFor(e.Format) {
			if !slices.Contains(e.Tried, s.Name()) {
				return GapEntry{}, false
			}
		}
	}
	return e, true
}

func (g *Gaps) resolve(key string, b Book, a *Author) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, had := g.entries[key]
	_, wasPending := g.pending[key]
	if !had && !wasPending {
		return
	}
	delete(g.entries, key)
	delete(g.pending, key) // a queued copy is skipped by the worker
	format := key[strings.IndexByte(key, '/')+1:]
	g.resolved[key] = Resolved{BookID: b.ID, Format: format, Title: b.Title, Slug: b.Slug, AuthorID: a.ID,
		AuthorName: a.Name, WasFound: had && e.Found(), ResolvedAt: time.Now()}
	g.dirty = true
}

func (g *Gaps) forget(key string) {
	g.mu.Lock()
	if _, ok := g.entries[key]; ok {
		delete(g.entries, key)
		g.dirty = true
	}
	delete(g.pending, key)
	g.mu.Unlock()
}

func (g *Gaps) requeue(j gapJob) {
	select {
	case g.queues[j.Format] <- j:
	default:
		g.mu.Lock()
		delete(g.pending, j.key())
		g.mu.Unlock()
	}
}

func (g *Gaps) worker(format string, queue chan gapJob) {
	for job := range queue {
		g.mu.Lock()
		_, still := g.pending[job.key()]
		g.mu.Unlock()
		if !still {
			continue // resolved or dropped since it was queued
		}
		g.lookup(job)
	}
}

func (g *Gaps) lookup(job gapJob) {
	var (
		tried      []string
		pausedTill time.Time
		lastErr    error
	)
	for _, s := range g.sourcesFor(job.Format) {
		g.mu.Lock()
		until := g.paused[s.Name()]
		g.mu.Unlock()
		if time.Now().Before(until) {
			if pausedTill.IsZero() || until.Before(pausedTill) {
				pausedTill = until
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		eds, err := s.Lookup(ctx, job.Title, job.Author, job.Format)
		cancel()
		if p, ok := asPaused(err); ok {
			slog.Warn("source paused", "source", s.Name(), "reason", p.Reason, "until", p.Until.Format(time.RFC3339))
			g.mu.Lock()
			g.paused[s.Name()] = p.Until
			g.dirty = true
			g.mu.Unlock()
			if pausedTill.IsZero() || p.Until.Before(pausedTill) {
				pausedTill = p.Until
			}
			continue
		}
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", s.Name(), err)
			continue
		}
		tried = append(tried, s.Name())
		if len(eds) > 0 {
			g.store(job, eds, s.Name(), tried)
			slog.Info("edition missing on Hardcover", "format", job.Format, "title", job.Title, "author", job.Author,
				"found", s.Name(), "id", eds[0].ID, "identifier", eds[0].Identifier())
			return
		}
	}
	switch {
	case lastErr == nil && pausedTill.IsZero():
		g.store(job, nil, "", tried) // every source answered: nothing exists
	case lastErr == nil:
		// Only paused sources are left: ask them once they're back,
		// without using up the job's retries.
		time.AfterFunc(time.Until(pausedTill)+time.Minute, func() { g.requeue(job) })
	case job.Attempt < gapMaxRetries:
		job.Attempt++
		slog.Warn("edition lookup failed; retrying later", "title", job.Title, "format", job.Format, "attempt", job.Attempt, "err", lastErr)
		time.AfterFunc(gapRetryDelay, func() { g.requeue(job) })
	default:
		slog.Warn("edition lookup gave up for now; the next refresh retries", "title", job.Title, "format", job.Format, "err", lastErr)
		g.mu.Lock()
		delete(g.pending, job.key())
		g.mu.Unlock()
	}
}

func (g *Gaps) store(job gapJob, eds []Edition, source string, tried []string) {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	prev := g.entries[job.key()]
	e := GapEntry{BookID: job.BookID, Format: job.Format, Title: job.Title, Slug: job.Slug, AuthorID: job.AuthorID,
		AuthorName: job.Author, Editions: eds, Source: source, Tried: tried, Checked: now, Expires: now.Add(gapMissTTL)}
	if e.Found() {
		e.Expires = now.Add(gapHitTTL)
		e.FoundAt = prev.FoundAt
		if e.FoundAt.IsZero() {
			e.FoundAt = now
		}
	}
	g.entries[job.key()] = e
	delete(g.pending, job.key())
	g.dirty = true
}

// ---- Reading state ----

// Status is the state of one format of one book.
type Status struct {
	State    string    `json:"state"`            // on-hardcover | missing | none-found | pending | not-checked
	Reason   string    `json:"reason,omitempty"` // why not-checked
	Count    int       `json:"count,omitempty"`  // editions on Hardcover
	Editions []Edition `json:"editions,omitempty"`
	Source   string    `json:"source,omitempty"`
	Checked  time.Time `json:"checked,omitempty"`
}

const (
	StateOnHardcover = "on-hardcover"
	StateMissing     = "missing" // not on Hardcover, found elsewhere
	StateNoneFound   = "none-found"
	StatePending     = "pending"
	StateNotChecked  = "not-checked"
)

func (g *Gaps) Status(b Book, format string) Status {
	switch {
	case b.Has(format):
		eds, n := b.Ebook, b.EbookCount
		if format == FormatAudio {
			eds, n = b.Audio, b.AudioCount
		}
		if n < len(eds) {
			n = len(eds) // snapshots from before counts existed
		}
		return Status{State: StateOnHardcover, Editions: eds, Source: "hardcover", Count: n}
	case format == FormatPrint:
		return Status{State: StateNotChecked}
	}
	key := gapKey(b.ID, format)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.pending[key]; ok {
		return Status{State: StatePending}
	}
	if e, ok := g.entries[key]; ok {
		if e.Found() {
			return Status{State: StateMissing, Editions: e.Editions, Source: e.Source, Checked: e.Checked}
		}
		return Status{State: StateNoneFound, Checked: e.Checked}
	}
	return Status{State: StateNotChecked, Reason: g.skipped[key]}
}

// Found lists every "missing on Hardcover" result for the given authors
// (all authors when ids is nil).
func (g *Gaps) Found(ids map[int]bool) []GapEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []GapEntry
	for _, e := range g.entries {
		if e.Found() && (ids == nil || ids[e.AuthorID]) {
			out = append(out, e)
		}
	}
	return out
}

// RecentlyResolved lists gaps Hardcover filled within d.
func (g *Gaps) RecentlyResolved(ids map[int]bool, d time.Duration) []Resolved {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Resolved
	for _, r := range g.resolved {
		if time.Since(r.ResolvedAt) < d && (ids == nil || ids[r.AuthorID]) {
			out = append(out, r)
		}
	}
	return out
}

// QueueStatus summarises outstanding work for the status bar.
type QueueStatus struct {
	Pending map[string]int       `json:"pending"`
	Paused  map[string]time.Time `json:"paused"`
	Cached  int                  `json:"cached"`
}

func (g *Gaps) Queue() QueueStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	q := QueueStatus{Pending: map[string]int{}, Paused: map[string]time.Time{}, Cached: len(g.entries)}
	for _, j := range g.pending {
		q.Pending[j.Format]++
	}
	for k, v := range g.paused {
		if time.Now().Before(v) {
			q.Paused[k] = v
		}
	}
	return q
}

// PendingFor counts queued lookups for one author.
func (g *Gaps) PendingFor(authorID int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, j := range g.pending {
		if j.AuthorID == authorID {
			n++
		}
	}
	return n
}

// ForgetAuthor drops an author's queued lookups (results stay cached).
func (g *Gaps) ForgetAuthor(authorID int) {
	g.mu.Lock()
	for k, j := range g.pending {
		if j.AuthorID == authorID {
			delete(g.pending, k)
		}
	}
	g.dirty = true
	g.mu.Unlock()
}

// Flush writes state to disk.
func (g *Gaps) Flush() {
	if g.dir == "" {
		return
	}
	g.mu.Lock()
	for k, r := range g.resolved {
		if time.Since(r.ResolvedAt) > resolvedKeep {
			delete(g.resolved, k)
		}
	}
	st := gapState{Paused: g.paused, Jobs: make([]gapJob, 0, len(g.pending))}
	for _, j := range g.pending {
		st.Jobs = append(st.Jobs, j)
	}
	entries, _ := json.Marshal(g.entries)
	resolved, _ := json.Marshal(g.resolved)
	queue, _ := json.Marshal(st)
	g.dirty = false
	g.mu.Unlock()
	writeAtomic(filepath.Join(g.dir, "gaps.json"), entries)
	writeAtomic(filepath.Join(g.dir, "gaps-resolved.json"), resolved)
	writeAtomic(filepath.Join(g.dir, "gaps-queue.json"), queue)
}

func readJSON(path string, out any) {
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, out); err != nil {
			slog.Warn("ignoring unreadable state file", "path", path, "err", err)
		}
	}
}

func writeAtomic(path string, raw []byte) {
	if raw == nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		slog.Warn("state write failed", "path", path, "err", err)
		return
	}
	_ = os.Rename(tmp, path)
}
