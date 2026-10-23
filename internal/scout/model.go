package scout

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Formats an edition can have.
const (
	FormatEbook = "ebook"
	FormatAudio = "audiobook"
	FormatPrint = "print"
)

// GapFormats are the formats looked up elsewhere when Hardcover lacks them.
var GapFormats = []string{FormatEbook, FormatAudio}

var ErrNotFound = errors.New("not found")

// Edition is one edition, on Hardcover or found elsewhere.
type Edition struct {
	Source        string   `json:"source"` // hardcover | audible | goodreads | googlebooks | openlibrary
	ID            string   `json:"id"`
	Format        string   `json:"format"`
	EditionFormat string   `json:"editionFormat,omitempty"` // as the source labels it: "Kindle Edition", "unabridged"
	Title         string   `json:"title"`
	Language      string   `json:"language,omitempty"`
	Publisher     string   `json:"publisher,omitempty"`
	Date          string   `json:"date,omitempty"`
	ISBN13        string   `json:"isbn13,omitempty"`
	ISBN10        string   `json:"isbn10,omitempty"`
	ASIN          string   `json:"asin,omitempty"`
	Pages         int      `json:"pages,omitempty"`
	DurationMin   int      `json:"durationMin,omitempty"`
	Narrators     []string `json:"narrators,omitempty"`
	// Other identifiers the source knows: Goodreads, Open Library and
	// Google Books ids, StoryGraph, OverDrive, Wikidata, ISFDB, OCLC, ...
	IDs      []Ident `json:"identifiers,omitempty"`
	URL      string  `json:"url,omitempty"`
	CoverURL string  `json:"coverUrl,omitempty"`
	Readers  int     `json:"readers,omitempty"` // Hardcover only
}

// Ident is one identifier of an edition.
type Ident struct {
	Kind  string `json:"kind"` // goodreads, openlibrary, googlebooks, storygraph, overdrive, wikidata, isfdb, librarything, musicbrainz, oclc, lccn
	Value string `json:"value"`
}

// addID records an identifier once.
func (e *Edition) addID(kind, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	for _, x := range e.IDs {
		if x.Kind == kind && x.Value == value {
			return
		}
	}
	e.IDs = append(e.IDs, Ident{kind, value})
}

// LabeledID is an identifier ready to show, with a link when it has a
// public page.
type LabeledID struct {
	Label string `json:"label"`
	Value string `json:"value"`
	URL   string `json:"url,omitempty"`
}

var idKinds = map[string]struct{ label, url string }{
	"goodreads":    {"Goodreads", "https://www.goodreads.com/book/show/%s"},
	"openlibrary":  {"Open Library", "https://openlibrary.org/books/%s"},
	"googlebooks":  {"Google Books", "https://books.google.com/books?id=%s"},
	"storygraph":   {"StoryGraph", "https://app.thestorygraph.com/books/%s"},
	"wikidata":     {"Wikidata", "https://www.wikidata.org/wiki/%s"},
	"isfdb":        {"ISFDB", "https://www.isfdb.org/cgi-bin/pl.cgi?%s"},
	"librarything": {"LibraryThing", "https://www.librarything.com/work/%s"},
	"musicbrainz":  {"MusicBrainz", "https://musicbrainz.org/release/%s"},
	"oclc":         {"OCLC", "https://search.worldcat.org/title/%s"},
	"lccn":         {"LCCN", "https://lccn.loc.gov/%s"},
	"overdrive":    {"OverDrive", ""},
}

// Identifiers lists every identifier: ISBN-13, ISBN-10, ASIN, then the
// others in the order the source gave them.
func (e Edition) Identifiers() []LabeledID {
	var out []LabeledID
	if e.ISBN13 != "" {
		out = append(out, LabeledID{"ISBN-13", e.ISBN13, ""})
	}
	if e.ISBN10 != "" {
		out = append(out, LabeledID{"ISBN-10", e.ISBN10, ""})
	}
	if e.ASIN != "" {
		u := "https://www.amazon.com/dp/" + e.ASIN
		if e.Format == FormatAudio {
			u = "https://www.audible.com/pd/" + e.ASIN
		}
		out = append(out, LabeledID{"ASIN", e.ASIN, u})
	}
	for _, x := range e.IDs {
		l := LabeledID{Label: x.Kind, Value: x.Value}
		if k, ok := idKinds[x.Kind]; ok {
			l.Label = k.label
			if k.url != "" {
				l.URL = fmt.Sprintf(k.url, url.PathEscape(x.Value))
			}
		}
		out = append(out, l)
	}
	return out
}

// IDValue returns the first identifier of a kind, for CSV columns.
func (e Edition) IDValue(kind string) string {
	for _, x := range e.IDs {
		if x.Kind == kind {
			return x.Value
		}
	}
	return ""
}

// Identifier lists the edition's ISBN and ASIN for display ("ISBN … ·
// ASIN …"), preferring ISBN-13 over ISBN-10.
func (e Edition) Identifier() string {
	var parts []string
	if isbn := e.ISBN13; isbn != "" || e.ISBN10 != "" {
		if isbn == "" {
			isbn = e.ISBN10
		}
		parts = append(parts, "ISBN "+isbn)
	}
	if e.ASIN != "" {
		parts = append(parts, "ASIN "+e.ASIN)
	}
	return strings.Join(parts, " · ")
}

// Duration formats DurationMin as "12 h 5 min".
func (e Edition) Duration() string {
	if e.DurationMin <= 0 {
		return ""
	}
	if e.DurationMin < 60 {
		return fmt.Sprintf("%d min", e.DurationMin)
	}
	return fmt.Sprintf("%d h %d min", e.DurationMin/60, e.DurationMin%60)
}

// Book is a Hardcover work and its English (or configured-language)
// editions there.
type Book struct {
	ID          int       `json:"id"`
	Title       string    `json:"title"`
	Subtitle    string    `json:"subtitle,omitempty"`
	Slug        string    `json:"slug"`
	Year        int       `json:"year,omitempty"`
	Readers     int       `json:"readers"`
	Compilation bool      `json:"compilation,omitempty"`
	Series      string    `json:"series,omitempty"`
	SeriesPos   string    `json:"seriesPosition,omitempty"`
	CoverURL    string    `json:"coverUrl,omitempty"`
	Ebook       []Edition `json:"ebook"`     // the most-read few
	Audio       []Edition `json:"audiobook"` // the most-read few
	HasPrint    bool      `json:"hasPrint"`
	// Editions per format on Hardcover (in the configured language or none).
	EbookCount int `json:"ebookEditions"`
	AudioCount int `json:"audiobookEditions"`
	PrintCount int `json:"printEditions"`
}

func (b Book) URL() string { return HardcoverBookURL(b.Slug, b.ID) }

func (b Book) Has(format string) bool {
	switch format {
	case FormatEbook:
		return len(b.Ebook) > 0
	case FormatAudio:
		return len(b.Audio) > 0
	case FormatPrint:
		return b.HasPrint
	}
	return false
}

func HardcoverBookURL(slug string, id int) string {
	if slug != "" {
		return "https://hardcover.app/books/" + slug
	}
	return fmt.Sprintf("https://hardcover.app/books/%d", id)
}

// Author is a built Hardcover author catalogue.
type Author struct {
	ID       int       `json:"id"`
	Name     string    `json:"name"`
	Slug     string    `json:"slug"`
	Bio      string    `json:"bio,omitempty"`
	ImageURL string    `json:"imageUrl,omitempty"`
	Books    []Book    `json:"books"`
	Built    time.Time `json:"built"`
}

func (a Author) URL() string {
	if a.Slug != "" {
		return "https://hardcover.app/authors/" + a.Slug
	}
	return fmt.Sprintf("https://hardcover.app/authors/%d", a.ID)
}

// AuthorHit is an author search result.
type AuthorHit struct {
	ID         int      `json:"id"`
	Name       string   `json:"name"`
	BooksCount int      `json:"booksCount"`
	ImageURL   string   `json:"imageUrl,omitempty"`
	KnownFor   []string `json:"knownFor,omitempty"`
}

// Lang describes the configured edition language for each source.
type Lang struct {
	Code2   string // Hardcover, Google Books
	Code3   string // OpenLibrary
	Name    string // Goodreads ("English")
	Audible string // Audible ("english")
}

var languages = map[string]Lang{
	"en": {"en", "eng", "English", "english"},
	"de": {"de", "ger", "German", "german"},
	"fr": {"fr", "fre", "French", "french"},
	"es": {"es", "spa", "Spanish", "spanish"},
	"it": {"it", "ita", "Italian", "italian"},
	"nl": {"nl", "dut", "Dutch", "dutch"},
	"pt": {"pt", "por", "Portuguese", "portuguese"},
}

func LookupLanguage(code string) (Lang, bool) {
	l, ok := languages[strings.ToLower(strings.TrimSpace(code))]
	return l, ok
}
