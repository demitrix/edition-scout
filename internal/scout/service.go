package scout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Service ties the catalogue, the edition lookups and the list of authors
// the user follows ("tracked" authors: every author opened, until removed).
type Service struct {
	Catalog *Catalog
	Gaps    *Gaps
	// MaxAge: tracked authors are rebuilt from Hardcover when older.
	MaxAge time.Duration
	// MinReaders: print-only books are looked up from this many readers.
	MinReaders int

	dir string

	mu       sync.Mutex
	tracked  map[int]Tracked
	authors  map[int]*Author // in-memory copies of built catalogues
	building map[int]*build
}

// Tracked is an author the user follows.
type Tracked struct {
	ID       int       `json:"id"`
	Name     string    `json:"name"`
	Added    time.Time `json:"added"`
	Built    time.Time `json:"built,omitempty"`
	NotFound bool      `json:"notFound,omitempty"`
}

type build struct {
	done   chan struct{}
	author *Author
	err    error
}

func NewService(c *Catalog, g *Gaps, dir string, maxAge time.Duration, minReaders int) *Service {
	s := &Service{Catalog: c, Gaps: g, MaxAge: maxAge, MinReaders: minReaders, dir: dir,
		tracked: map[int]Tracked{}, authors: map[int]*Author{}, building: map[int]*build{}}
	if dir != "" {
		_ = os.MkdirAll(filepath.Join(dir, "authors"), 0o755)
		readJSON(filepath.Join(dir, "tracked.json"), &s.tracked)
	}
	return s
}

func (s *Service) snapshotPath(id int) string {
	return filepath.Join(s.dir, "authors", fmt.Sprintf("%d.json", id))
}

func (s *Service) saveTracked() {
	if s.dir == "" {
		return
	}
	s.mu.Lock()
	raw, _ := json.Marshal(s.tracked)
	s.mu.Unlock()
	writeAtomic(filepath.Join(s.dir, "tracked.json"), raw)
}

// cached returns the last built catalogue, from memory or disk.
func (s *Service) cached(id int) *Author {
	s.mu.Lock()
	a := s.authors[id]
	s.mu.Unlock()
	if a != nil || s.dir == "" {
		return a
	}
	var disk Author
	if raw, err := os.ReadFile(s.snapshotPath(id)); err == nil && json.Unmarshal(raw, &disk) == nil && disk.ID == id {
		s.mu.Lock()
		s.authors[id] = &disk
		s.mu.Unlock()
		return &disk
	}
	return nil
}

// Author returns an author's catalogue, rebuilding it from Hardcover when
// it's older than MaxAge or force is set. Building continues in the
// background if ctx ends first; ErrBuilding is returned then (and stale is
// the last copy, if any).
func (s *Service) Author(ctx context.Context, id int, force bool) (a *Author, err error) {
	if c := s.cached(id); c != nil && !force && time.Since(c.Built) < s.MaxAge {
		return c, nil
	}
	b := s.startBuild(id)
	select {
	case <-b.done:
		return b.author, b.err
	case <-ctx.Done():
		return s.cached(id), ErrBuilding
	}
}

var ErrBuilding = errors.New("still building")

// Building reports whether an author build is in progress.
func (s *Service) Building(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.building[id]
	return ok
}

func (s *Service) startBuild(id int) *build {
	s.mu.Lock()
	if b, ok := s.building[id]; ok {
		s.mu.Unlock()
		return b
	}
	b := &build{done: make(chan struct{})}
	s.building[id] = b
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		start := time.Now()
		a, err := s.Catalog.BuildAuthor(ctx, id)
		if err == nil {
			queued := s.Gaps.Process(a, s.MinReaders)
			slog.Info("author built", "id", id, "name", a.Name, "books", len(a.Books), "lookupsQueued", queued,
				"ms", time.Since(start).Milliseconds())
			raw, _ := json.Marshal(a)
			if s.dir != "" {
				writeAtomic(s.snapshotPath(id), raw)
			}
		} else {
			slog.Warn("author build failed", "id", id, "err", err)
		}
		s.mu.Lock()
		if err == nil {
			s.authors[id] = a
			if t, ok := s.tracked[id]; ok {
				t.Name, t.Built, t.NotFound = a.Name, a.Built, false
				s.tracked[id] = t
			}
		} else if errors.Is(err, ErrNotFound) {
			if t, ok := s.tracked[id]; ok {
				t.NotFound, t.Built = true, time.Now()
				s.tracked[id] = t
			}
		}
		delete(s.building, id)
		s.mu.Unlock()
		s.saveTracked()
		b.author, b.err = a, err
		close(b.done)
	}()
	return b
}

// Track adds an author to the followed list.
func (s *Service) Track(id int, name string) {
	s.mu.Lock()
	if _, ok := s.tracked[id]; !ok {
		s.tracked[id] = Tracked{ID: id, Name: name, Added: time.Now()}
	}
	s.mu.Unlock()
	s.saveTracked()
}

// Untrack removes an author and their queued lookups (cached results stay).
func (s *Service) Untrack(id int) {
	s.mu.Lock()
	delete(s.tracked, id)
	delete(s.authors, id)
	s.mu.Unlock()
	s.Gaps.ForgetAuthor(id)
	if s.dir != "" {
		_ = os.Remove(s.snapshotPath(id))
	}
	s.saveTracked()
}

func (s *Service) IsTracked(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.tracked[id]
	return ok
}

// TrackedAuthors lists followed authors by name.
func (s *Service) TrackedAuthors() []Tracked {
	s.mu.Lock()
	out := make([]Tracked, 0, len(s.tracked))
	for _, t := range s.tracked {
		out = append(out, t)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (s *Service) trackedIDs() map[int]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := map[int]bool{}
	for id := range s.tracked {
		ids[id] = true
	}
	return ids
}

// Refresh keeps tracked authors current, one at a time, oldest first, so
// Hardcover sees a slow trickle and gaps Hardcover fills are noticed.
func (s *Service) Refresh(ctx context.Context) {
	time.Sleep(time.Minute) // let startup settle
	for {
		for _, t := range s.TrackedAuthors() {
			due := time.Since(t.Built) > s.MaxAge
			if t.NotFound {
				due = time.Since(t.Built) > 7*24*time.Hour
			}
			if !due {
				continue
			}
			b := s.startBuild(t.ID)
			select {
			case <-b.done:
			case <-ctx.Done():
				return
			}
			time.Sleep(5 * time.Second)
		}
		select {
		case <-time.After(10 * time.Minute):
		case <-ctx.Done():
			return
		}
	}
}

// ---- Views ----

// BookView is one catalogue row with the state of each format.
type BookView struct {
	Book
	Ebook Status `json:"ebookStatus"`
	Audio Status `json:"audiobookStatus"`
}

// Counts summarise an author's catalogue.
type Counts struct {
	Books        int `json:"books"`
	MissingEbook int `json:"missingEbook"`
	MissingAudio int `json:"missingAudiobook"`
	Pending      int `json:"pending"`
	NoneFound    int `json:"noneFound"`
}

func (s *Service) View(a *Author) ([]BookView, Counts) {
	books := append([]Book(nil), a.Books...)
	SortBooks(books)
	rows := make([]BookView, 0, len(books))
	var c Counts
	for _, b := range books {
		v := BookView{Book: b, Ebook: s.Gaps.Status(b, FormatEbook), Audio: s.Gaps.Status(b, FormatAudio)}
		rows = append(rows, v)
		c.Books++
		for i, st := range []Status{v.Ebook, v.Audio} {
			switch st.State {
			case StateMissing:
				if i == 0 {
					c.MissingEbook++
				} else {
					c.MissingAudio++
				}
			case StatePending:
				c.Pending++
			case StateNoneFound:
				c.NoneFound++
			}
		}
	}
	return rows, c
}

// ReportRow is one "missing on Hardcover" finding.
type ReportRow struct {
	GapEntry
	HardcoverURL string `json:"hardcoverUrl"`
}

// Report lists everything found elsewhere for tracked authors, by author
// then title, and gaps Hardcover filled in the last two weeks.
func (s *Service) Report() ([]ReportRow, []Resolved) {
	ids := s.trackedIDs()
	var rows []ReportRow
	for _, e := range s.Gaps.Found(ids) {
		rows = append(rows, ReportRow{GapEntry: e, HardcoverURL: HardcoverBookURL(e.Slug, e.BookID)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].AuthorName != rows[j].AuthorName {
			return strings.ToLower(rows[i].AuthorName) < strings.ToLower(rows[j].AuthorName)
		}
		if rows[i].Title != rows[j].Title {
			return strings.ToLower(rows[i].Title) < strings.ToLower(rows[j].Title)
		}
		return rows[i].Format < rows[j].Format
	})
	resolved := s.Gaps.RecentlyResolved(ids, 14*24*time.Hour)
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].ResolvedAt.After(resolved[j].ResolvedAt) })
	return rows, resolved
}

// Summary is a per-tracked-author line for the home page.
type Summary struct {
	Tracked
	Counts
	Building bool `json:"building"`
}

func (s *Service) Summaries() []Summary {
	var out []Summary
	for _, t := range s.TrackedAuthors() {
		sum := Summary{Tracked: t, Building: s.Building(t.ID)}
		if a := s.cached(t.ID); a != nil {
			_, sum.Counts = s.View(a)
		}
		out = append(out, sum)
	}
	return out
}
