package scout

import (
	"errors"
	"fmt"
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
	URL           string   `json:"url,omitempty"`
	CoverURL      string   `json:"coverUrl,omitempty"`
	Readers       int      `json:"readers,omitempty"` // Hardcover only
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
