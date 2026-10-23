package scout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

func urlQuery(s string) string { return url.QueryEscape(s) }

var ebookFormat = regexp.MustCompile(`(?i)\b(e-?book|electronic|kindle|epub|digital|nook|kobo)\b`)

// ---- Audible (audiobooks) ----

// Audible's public catalogue API: the JSON endpoint the Audible apps and
// many audiobook tools use. No key, no page scraping.
type Audible struct {
	Region string // us, uk, ca, au, de, fr, it, es, in, jp
	Lang   Lang
	http   *http.Client
	p      pacer
}

var audibleTLD = map[string]string{
	"us": "com", "uk": "co.uk", "ca": "ca", "au": "com.au", "de": "de", "fr": "fr",
	"it": "it", "es": "es", "in": "in", "jp": "co.jp",
}

func NewAudible(region string, lang Lang) (*Audible, error) {
	region = strings.ToLower(strings.TrimSpace(region))
	if _, ok := audibleTLD[region]; !ok {
		return nil, fmt.Errorf("unknown Audible region %q", region)
	}
	return &Audible{Region: region, Lang: lang, http: &http.Client{Timeout: 20 * time.Second}, p: pacer{interval: time.Second}}, nil
}

func (a *Audible) Name() string                { return "audible" }
func (a *Audible) Supports(format string) bool { return format == FormatAudio }

func (a *Audible) Lookup(ctx context.Context, title, author, _ string) ([]Edition, error) {
	tld := audibleTLD[a.Region]
	u := "https://api.audible." + tld + "/1.0/catalog/products?num_results=20&products_sort_by=Relevance" +
		"&response_groups=contributors,product_attrs,product_details,media&title=" + urlQuery(title) + "&author=" + urlQuery(author)
	var resp struct {
		Products []struct {
			ASIN     string `json:"asin"`
			Title    string `json:"title"`
			Subtitle string `json:"subtitle"`
			Authors  []struct {
				Name string `json:"name"`
			} `json:"authors"`
			Narrators []struct {
				Name string `json:"name"`
			} `json:"narrators"`
			ISBN        string            `json:"isbn"` // the audiobook's own; not every title has one
			Publisher   string            `json:"publisher_name"`
			ReleaseDate string            `json:"release_date"`
			Runtime     int               `json:"runtime_length_min"`
			Language    string            `json:"language"`
			FormatType  string            `json:"format_type"` // unabridged, abridged
			ContentType string            `json:"content_type"`
			Images      map[string]string `json:"product_images"`
		} `json:"products"`
	}
	if err := getJSON(ctx, a.http, &a.p, u, toolUA, &resp); err != nil {
		var se *httpStatusError
		if errors.As(err, &se) && se.Status == http.StatusTooManyRequests {
			return nil, &PausedError{Until: time.Now().Add(15 * time.Minute), Reason: "Audible rate limited"}
		}
		return nil, err
	}
	var out []Edition
	for _, p := range resp.Products {
		names := make([]string, 0, len(p.Authors))
		for _, au := range p.Authors {
			names = append(names, au.Name)
		}
		if !strings.EqualFold(p.Language, a.Lang.Audible) || !titleMatches(p.Title, title) || !authorCredited(names, author) {
			continue
		}
		// Podcast episodes share the catalogue; radio dramas and
		// performances are real audiobook editions.
		if strings.EqualFold(p.ContentType, "Podcast") || strings.EqualFold(p.ContentType, "Episode") {
			continue
		}
		ed := Edition{Source: a.Name(), ID: p.ASIN, Format: FormatAudio, EditionFormat: p.FormatType, Title: p.Title,
			Language: a.Lang.Name, Publisher: p.Publisher, Date: p.ReleaseDate, ASIN: p.ASIN, DurationMin: p.Runtime,
			URL: "https://www.audible." + tld + "/pd/" + p.ASIN}
		switch isbn := strings.ReplaceAll(p.ISBN, "-", ""); len(isbn) {
		case 13:
			ed.ISBN13 = isbn
		case 10:
			ed.ISBN10 = isbn
		}
		for _, n := range p.Narrators {
			ed.Narrators = append(ed.Narrators, n.Name)
		}
		if img := p.Images["500"]; img != "" {
			ed.CoverURL = img
		}
		out = append(out, ed)
		if len(out) == 3 {
			break
		}
	}
	return out, nil
}

// ---- Goodreads (ebooks and audiobooks, opt-in) ----

// Goodreads has had no public API since 2020. This reads the two page types
// that load without a bot check from a normal browser user agent: the search
// autocomplete (JSON) and a work's editions list. Book and search pages are
// bot-checked and never used. Requests are serialised, one every 4 seconds,
// results are cached briefly so both formats share one fetch, and a bot
// check pauses Goodreads for 30 minutes.
type Goodreads struct {
	Lang Lang
	http *http.Client
	p    pacer

	mu    sync.Mutex
	cache map[string]grCached
}

type grCached struct {
	ebook, audio []Edition
	at           time.Time
}

const (
	grUserAgent   = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
	grBlockedWait = 30 * time.Minute
	grCacheTTL    = 6 * time.Hour
)

func NewGoodreads(lang Lang) *Goodreads {
	return &Goodreads{Lang: lang, http: &http.Client{Timeout: 45 * time.Second}, p: pacer{interval: 4 * time.Second}, cache: map[string]grCached{}}
}

func (g *Goodreads) Name() string { return "goodreads" }
func (g *Goodreads) Supports(format string) bool {
	return format == FormatEbook || format == FormatAudio
}

func (g *Goodreads) get(ctx context.Context, u string) ([]byte, error) {
	body, status, err := fetch(ctx, g.http, &g.p, u, map[string]string{
		"User-Agent": grUserAgent, "Accept": "text/html,application/json", "Accept-Language": "en-US,en;q=0.9",
	})
	if err != nil {
		return nil, err
	}
	if status == http.StatusAccepted || (status == http.StatusOK && len(body) == 0) {
		return nil, &PausedError{Until: time.Now().Add(grBlockedWait), Reason: "Goodreads bot check"}
	}
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if status != http.StatusOK {
		return nil, &httpStatusError{status, shorten(string(body))}
	}
	return body, nil
}

func (g *Goodreads) Lookup(ctx context.Context, title, author, format string) ([]Edition, error) {
	key := normalize(title) + "|" + normalize(author)
	g.mu.Lock()
	c, ok := g.cache[key]
	g.mu.Unlock()
	if !ok || time.Since(c.at) > grCacheTTL {
		ebook, audio, err := g.editionsFor(ctx, title, author)
		if err != nil {
			return nil, err
		}
		c = grCached{ebook, audio, time.Now()}
		g.mu.Lock()
		if len(g.cache) > 5000 {
			g.cache = map[string]grCached{}
		}
		g.cache[key] = c
		g.mu.Unlock()
	}
	if format == FormatAudio {
		return c.audio, nil
	}
	return c.ebook, nil
}

func (g *Goodreads) editionsFor(ctx context.Context, title, author string) (ebook, audio []Edition, err error) {
	body, err := g.get(ctx, "https://www.goodreads.com/book/auto_complete?format=json&q="+urlQuery(title+" "+author))
	if err != nil {
		return nil, nil, err
	}
	var hits []struct {
		WorkID        string `json:"workId"`
		Title         string `json:"title"`
		BookTitleBare string `json:"bookTitleBare"`
		Author        struct {
			Name string `json:"name"`
		} `json:"author"`
		RatingsCount int `json:"ratingsCount"`
	}
	if err := json.Unmarshal(body, &hits); err != nil {
		return nil, nil, err
	}
	best := -1
	for i, h := range hits {
		bare := h.BookTitleBare
		if bare == "" {
			bare = h.Title
		}
		if normalize(h.Author.Name) != normalize(author) || !titleMatches(bare, title) {
			continue
		}
		if best < 0 || h.RatingsCount > hits[best].RatingsCount {
			best = i
		}
	}
	if best < 0 {
		return nil, nil, nil
	}
	return g.editions(ctx, hits[best].WorkID, title)
}

var (
	grEditionBlock = regexp.MustCompile(`<div class="elementList clearFix"`)
	grBookLink     = regexp.MustCompile(`href="/book/show/(\d+)[^"]*" class="bookTitle"[^>]*>\s*([^<]+)`)
	grBookLinkAlt  = regexp.MustCompile(`class="bookTitle"[^>]*href="/book/show/(\d+)[^"]*"[^>]*>\s*([^<]+)`)
	grFormatRow    = regexp.MustCompile(`(?is)<div class="dataRow">\s*([^<]*?(?:Kindle|ebook|e-book|Nook|Kobo|EPUB|Paperback|Hardcover|Audio|Audible|MP3|Mass Market|Library Binding|Board book)[^<]*)<`)
	grISBN13       = regexp.MustCompile(`(?s)ISBN13:\s*</div>\s*<div class="dataValue">\s*(\d{13})`)
	grISBNAny      = regexp.MustCompile(`(?s)ISBN:\s*</div>\s*<div class="dataValue">\s*(\d{13}|\d{9}[\dX])\b\s*(?:<span[^>]*>\s*\(ISBN13:\s*(\d{13})\)?)?`)
	grASIN         = regexp.MustCompile(`(?s)ASIN:\s*</div>\s*<div class="dataValue">\s*([A-Z0-9]{10})`)
	grLanguage     = regexp.MustCompile(`(?s)Edition language:\s*</div>\s*<div class="dataValue">\s*([^<\n]+)`)
	grPublished    = regexp.MustCompile(`(?s)Published\s*(?:<nobr[^>]*>)?\s*([A-Za-z]+\s+\d{1,2}(?:st|nd|rd|th)?\s+\d{4}|[A-Za-z]+\s+\d{4}|\d{4})?\s*(?:</nobr>)?\s*by\s*([^<\n]+)`)
	grCover        = regexp.MustCompile(`<img[^>]+src="(https://[^"]+/books/[^"]+)"`)
	grPages        = regexp.MustCompile(`(\d+)\s+pages`)
	grDigits       = regexp.MustCompile(`^\d{9}[\dX]$`) // an ISBN-10 in the ASIN field
	grOrdinal      = regexp.MustCompile(`(\d)(st|nd|rd|th)\b`)
	grFormatSuffix = regexp.MustCompile(`(?i)\s*\((?:kindle edition|ebook|audiobook|unabridged|audible audio(?: edition)?|audio cd|mp3 cd)\)$`)
	grAudioFormat  = regexp.MustCompile(`(?i)\b(audio|audible|audiobook|mp3)\b`)
)

func grDate(s string) string {
	s = grOrdinal.ReplaceAllString(strings.TrimSpace(s), "$1")
	for _, layout := range []string{"January 2 2006", "January 2006", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// stripGRSeries drops Goodreads' "(Series, #1)" suffix from a title.
func stripGRSeries(t string) string {
	if i := strings.LastIndex(t, " ("); i > 0 && strings.HasSuffix(t, ")") && strings.Contains(t[i:], "#") {
		return strings.TrimSpace(t[:i])
	}
	return t
}

// editions parses a work's editions page: editions in the configured
// language (or none recorded) whose own title agrees with wantTitle (a
// Goodreads work can hold editions of collections), up to 3 per format.
func (g *Goodreads) editions(ctx context.Context, workID, wantTitle string) (ebook, audio []Edition, err error) {
	body, err := g.get(ctx, "https://www.goodreads.com/work/editions/"+workID+"?per_page=100&expanded=true")
	if err != nil {
		return nil, nil, err
	}
	page := string(body)
	idx := grEditionBlock.FindAllStringIndex(page, -1)
	for i, loc := range idx {
		end := len(page)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		b := page[loc[0]:end]
		m := grBookLink.FindStringSubmatch(b)
		if m == nil {
			m = grBookLinkAlt.FindStringSubmatch(b)
		}
		if m == nil {
			continue
		}
		bookID := m[1]
		title := stripGRSeries(strings.TrimSpace(html.UnescapeString(m[2])))
		title = strings.TrimSpace(grFormatSuffix.ReplaceAllString(title, ""))
		if !titleMatches(title, wantTitle) {
			continue
		}
		format := ""
		if f := grFormatRow.FindStringSubmatch(b); f != nil {
			format = strings.TrimSpace(html.UnescapeString(f[1]))
		}
		publisher, date := "", ""
		if pub := grPublished.FindStringSubmatch(b); pub != nil {
			date, publisher = grDate(pub[1]), strings.TrimSpace(html.UnescapeString(pub[2]))
		}
		if l := grLanguage.FindStringSubmatch(b); l != nil && !strings.EqualFold(strings.TrimSpace(l[1]), g.Lang.Name) {
			continue
		}
		var kind string
		switch {
		case grAudioFormat.MatchString(format) || strings.Contains(publisher, "Audible"):
			kind = FormatAudio
		case ebookFormat.MatchString(format):
			kind = FormatEbook
		default:
			continue
		}
		if (kind == FormatAudio && len(audio) >= 3) || (kind == FormatEbook && len(ebook) >= 3) {
			continue
		}
		ed := Edition{Source: g.Name(), ID: bookID, Format: kind, EditionFormat: strings.TrimSpace(strings.Split(format, ",")[0]),
			Title: title, Language: g.Lang.Name, Publisher: publisher, Date: date,
			URL: "https://www.goodreads.com/book/show/" + bookID}
		if kind == FormatEbook {
			if p := grPages.FindStringSubmatch(format); p != nil {
				ed.Pages, _ = strconv.Atoi(p[1])
			}
		}
		if v := grISBN13.FindStringSubmatch(b); v != nil {
			ed.ISBN13 = v[1]
		} else if v := grISBNAny.FindStringSubmatch(b); v != nil {
			if len(v[1]) == 13 {
				ed.ISBN13 = v[1]
			} else {
				ed.ISBN10, ed.ISBN13 = v[1], v[2]
			}
		}
		if v := grASIN.FindStringSubmatch(b); v != nil && !grDigits.MatchString(v[1]) {
			ed.ASIN = v[1]
		}
		if v := grCover.FindStringSubmatch(b); v != nil {
			ed.CoverURL = v[1]
		}
		if grLanguage.FindStringSubmatch(b) == nil && !g.Lang.ISBNFits(ed.ISBN13, ed.ISBN10) {
			continue // no language shown, and the ISBN is from another language area
		}
		ed.addID("goodreads", bookID) // from /book/show/<id>-slug
		if kind == FormatAudio {
			audio = append(audio, ed)
		} else {
			ebook = append(ebook, ed)
		}
	}
	return ebook, audio, nil
}
