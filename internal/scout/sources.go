package scout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A Source finds editions of a book outside Hardcover.
type Source interface {
	Name() string
	Supports(format string) bool
	// Lookup returns matching editions in the configured language (none is
	// a valid answer). A *PausedError means the source can't be asked until
	// a given time (quota, bot check).
	Lookup(ctx context.Context, title, author, format string) ([]Edition, error)
}

// PausedError: the source is unavailable until Until.
type PausedError struct {
	Until  time.Time
	Reason string
}

func (e *PausedError) Error() string {
	return fmt.Sprintf("%s (until %s)", e.Reason, e.Until.Format(time.RFC3339))
}

func asPaused(err error) (*PausedError, bool) {
	var p *PausedError
	ok := errors.As(err, &p)
	return p, ok
}

// pacer spaces one source's requests.
type pacer struct {
	interval time.Duration
	mu       sync.Mutex
	next     time.Time
}

func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	start := time.Now()
	if p.next.After(start) {
		start = p.next
	}
	p.next = start.Add(p.interval)
	p.mu.Unlock()
	if d := time.Until(start); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// httpStatusError carries a non-200 answer.
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// fetch GETs u (paced), retrying 5xx answers a couple of times.
func fetch(ctx context.Context, client *http.Client, p *pacer, u string, headers map[string]string) ([]byte, int, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 3 * time.Second):
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
		}
		if err := p.wait(ctx); err != nil {
			return nil, 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, 0, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = &httpStatusError{resp.StatusCode, shorten(string(body))}
			continue
		}
		return body, resp.StatusCode, nil
	}
	return nil, 0, lastErr
}

func getJSON(ctx context.Context, client *http.Client, p *pacer, u string, ua string, out any) error {
	body, status, err := fetch(ctx, client, p, u, map[string]string{"User-Agent": ua, "Accept": "application/json"})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &httpStatusError{status, shorten(string(body))}
	}
	return json.Unmarshal(body, out)
}

func shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, "key="); i >= 0 {
		s = s[:i] + "key=…" // never log an API key
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

const toolUA = "edition-scout (self-hosted; +https://hardcover.app)"

// ---- Google Books (ebooks) ----

type GoogleBooks struct {
	Key  string
	Lang Lang
	http *http.Client
	p    pacer
}

func NewGoogleBooks(key string, lang Lang) *GoogleBooks {
	return &GoogleBooks{Key: key, Lang: lang, http: &http.Client{Timeout: 20 * time.Second}, p: pacer{interval: time.Second}}
}

func (g *GoogleBooks) Name() string                { return "googlebooks" }
func (g *GoogleBooks) Supports(format string) bool { return format == FormatEbook }

// nextGoogleReset: the daily quota resets at midnight US Pacific.
func nextGoogleReset(now time.Time) time.Time {
	pt, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return now.Add(24 * time.Hour)
	}
	t := now.In(pt)
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 5, 0, 0, pt)
}

func (g *GoogleBooks) Lookup(ctx context.Context, title, author, _ string) ([]Edition, error) {
	// Plain keywords: Google's intitle:/inauthor: operators miss books a
	// keyword search finds. Title, author, language and ebook status are
	// checked strictly below instead.
	u := "https://www.googleapis.com/books/v1/volumes?printType=books&maxResults=20&q=" +
		urlQuery(title+" "+author) + "&key=" + urlQuery(g.Key)
	var resp struct {
		Items []struct {
			ID         string `json:"id"`
			VolumeInfo struct {
				Title               string   `json:"title"`
				Authors             []string `json:"authors"`
				Publisher           string   `json:"publisher"`
				PublishedDate       string   `json:"publishedDate"`
				PageCount           int      `json:"pageCount"`
				Language            string   `json:"language"`
				IndustryIdentifiers []struct {
					Type       string `json:"type"`
					Identifier string `json:"identifier"`
				} `json:"industryIdentifiers"`
				ImageLinks struct {
					Thumbnail string `json:"thumbnail"`
				} `json:"imageLinks"`
			} `json:"volumeInfo"`
			SaleInfo struct {
				IsEbook bool `json:"isEbook"`
			} `json:"saleInfo"`
		} `json:"items"`
	}
	if err := getJSON(ctx, g.http, &g.p, u, toolUA, &resp); err != nil {
		var se *httpStatusError
		if errors.As(err, &se) && se.Status == http.StatusTooManyRequests && strings.Contains(se.Body, "Quota") {
			return nil, &PausedError{Until: nextGoogleReset(time.Now()), Reason: "Google Books daily quota used up"}
		}
		return nil, err
	}
	var out []Edition
	for _, it := range resp.Items {
		v := it.VolumeInfo
		if !it.SaleInfo.IsEbook || v.Language != g.Lang.Code2 || !titleMatches(v.Title, title) || !authorCredited(v.Authors, author) {
			continue
		}
		ed := Edition{Source: g.Name(), ID: it.ID, Format: FormatEbook, EditionFormat: "ebook", Title: v.Title,
			Language: g.Lang.Name, Publisher: v.Publisher, Date: v.PublishedDate, Pages: v.PageCount,
			URL: "https://books.google.com/books?id=" + it.ID, CoverURL: strings.Replace(v.ImageLinks.Thumbnail, "http://", "https://", 1)}
		for _, id := range v.IndustryIdentifiers {
			switch id.Type {
			case "ISBN_13":
				ed.ISBN13 = id.Identifier
			case "ISBN_10":
				ed.ISBN10 = id.Identifier
			}
		}
		out = append(out, ed)
		if len(out) == 3 {
			break
		}
	}
	return out, nil
}

// ---- OpenLibrary (ebooks) ----

type OpenLibrary struct {
	Lang Lang
	http *http.Client
	p    pacer
}

func NewOpenLibrary(lang Lang) *OpenLibrary {
	return &OpenLibrary{Lang: lang, http: &http.Client{Timeout: 20 * time.Second}, p: pacer{interval: time.Second}}
}

func (o *OpenLibrary) Name() string                { return "openlibrary" }
func (o *OpenLibrary) Supports(format string) bool { return format == FormatEbook }

func (o *OpenLibrary) Lookup(ctx context.Context, title, author, _ string) ([]Edition, error) {
	var search struct {
		Docs []struct {
			Key        string   `json:"key"`
			Title      string   `json:"title"`
			AuthorName []string `json:"author_name"`
		} `json:"docs"`
	}
	u := "https://openlibrary.org/search.json?fields=key,title,author_name&limit=5&title=" + urlQuery(title) + "&author=" + urlQuery(author)
	if err := getJSON(ctx, o.http, &o.p, u, toolUA, &search); err != nil {
		return nil, err
	}
	for _, d := range search.Docs {
		if !titleMatches(d.Title, title) || !authorCredited(d.AuthorName, author) {
			continue
		}
		var eds struct {
			Entries []struct {
				Key            string   `json:"key"`
				Title          string   `json:"title"`
				PhysicalFormat string   `json:"physical_format"`
				Publishers     []string `json:"publishers"`
				PublishDate    string   `json:"publish_date"`
				Isbn13         []string `json:"isbn_13"`
				Isbn10         []string `json:"isbn_10"`
				Pages          int      `json:"number_of_pages"`
				Covers         []int    `json:"covers"`
				Languages      []struct {
					Key string `json:"key"`
				} `json:"languages"`
			} `json:"entries"`
		}
		if err := getJSON(ctx, o.http, &o.p, "https://openlibrary.org"+d.Key+"/editions.json?limit=100", toolUA, &eds); err != nil {
			return nil, err
		}
		var out []Edition
		for _, e := range eds.Entries {
			// OpenLibrary often leaves formats blank: only editions explicitly
			// labelled as ebooks count.
			if !ebookFormat.MatchString(e.PhysicalFormat) {
				continue
			}
			ok := len(e.Languages) == 0
			for _, l := range e.Languages {
				ok = ok || l.Key == "/languages/"+o.Lang.Code3
			}
			if !ok {
				continue
			}
			ed := Edition{Source: o.Name(), ID: strings.TrimPrefix(e.Key, "/books/"), Format: FormatEbook,
				EditionFormat: e.PhysicalFormat, Title: e.Title, Language: o.Lang.Name, Date: e.PublishDate,
				ISBN13: firstOf(e.Isbn13), ISBN10: firstOf(e.Isbn10), Pages: e.Pages, URL: "https://openlibrary.org" + e.Key}
			if len(e.Publishers) > 0 {
				ed.Publisher = e.Publishers[0]
			}
			if len(e.Covers) > 0 && e.Covers[0] > 0 {
				ed.CoverURL = fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", e.Covers[0])
			}
			out = append(out, ed)
			if len(out) == 3 {
				break
			}
		}
		return out, nil // only the best-matching work
	}
	return nil, nil
}

func firstOf(xs []string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return ""
}
