package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"edition-scout/internal/hardcover"
)

// Catalog reads authors and their works from Hardcover.
type Catalog struct {
	HC   *hardcover.Client
	Lang Lang
	// IncludeCompilations keeps boxed sets and omnibus volumes.
	IncludeCompilations bool
}

// editionDetail: Hardcover editions read in full per format (most-read
// first); every edition is counted.
const editionDetail = 3

// SearchAuthors runs Hardcover's author search, dropping placeholder
// records ("Various", "Unknown").
func (c *Catalog) SearchAuthors(ctx context.Context, q string) ([]AuthorHit, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	var resp struct {
		Data struct {
			Search struct {
				Results json.RawMessage `json:"results"`
			} `json:"search"`
		} `json:"data"`
	}
	gql := `query Search($q: String!) { search(query: $q, query_type: "Author", per_page: 20) { results } }`
	if err := c.HC.Query(ctx, gql, map[string]any{"q": q}, &resp); err != nil {
		return nil, err
	}
	var results struct {
		Hits []struct {
			Document struct {
				ID         string   `json:"id"`
				Name       string   `json:"name"`
				BooksCount int      `json:"books_count"`
				Books      []string `json:"books"`
				Image      struct {
					URL string `json:"url"`
				} `json:"image"`
			} `json:"document"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(resp.Data.Search.Results, &results); err != nil {
		return nil, fmt.Errorf("hardcover search results: %w", err)
	}
	out := []AuthorHit{}
	for _, h := range results.Hits {
		d := h.Document
		id, err := strconv.Atoi(d.ID)
		if err != nil || IsPlaceholderAuthor(d.Name) {
			continue
		}
		var known []string
		for _, t := range d.Books {
			if len(known) == 3 {
				break
			}
			if r := []rune(t); len(r) > 60 {
				t = string(r[:57]) + "…"
			}
			known = append(known, t)
		}
		out = append(out, AuthorHit{ID: id, Name: d.Name, BooksCount: d.BooksCount, ImageURL: d.Image.URL, KnownFor: known})
	}
	return out, nil
}

type hcAuthor struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Bio   string `json:"bio"`
	Image *struct {
		URL string `json:"url"`
	} `json:"image"`
}

type hcEdition struct {
	ID            int    `json:"id"`
	Title         string `json:"title"`
	Asin          string `json:"asin"`
	Isbn13        string `json:"isbn_13"`
	Isbn10        string `json:"isbn_10"`
	EditionFormat string `json:"edition_format"`
	ReleaseDate   string `json:"release_date"`
	Pages         int    `json:"pages"`
	AudioSeconds  int    `json:"audio_seconds"`
	UsersCount    int    `json:"users_count"`
	Publisher     *struct {
		Name string `json:"name"`
	} `json:"publisher"`
	Language *struct {
		Language string `json:"language"`
	} `json:"language"`
	Contributions []struct {
		Contribution *string `json:"contribution"`
		Author       struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"contributions"`
}

type hcID struct {
	ID int `json:"id"`
}

type hcBook struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Subtitle    string `json:"subtitle"`
	Slug        string `json:"slug"`
	ReleaseYear int    `json:"release_year"`
	UsersCount  int    `json:"users_count"`
	Compilation bool   `json:"compilation"`
	Image       *struct {
		URL string `json:"url"`
	} `json:"image"`
	BookSeries []struct {
		Position *float64 `json:"position"`
		Featured bool     `json:"featured"`
		Series   struct {
			Name string `json:"name"`
		} `json:"series"`
	} `json:"book_series"`
	Audio    []hcEdition `json:"audio"`
	Ebook    []hcEdition `json:"ebook"`
	AudioIDs []hcID      `json:"audio_ids"`
	EbookIDs []hcID      `json:"ebook_ids"`
	PrintIDs []hcID      `json:"print_ids"`
}

const authorRoleFilter = `_or: [{contribution: {_is_null: true}}, {contribution: {_eq: ""}}, {contribution: {_in: ["Author", "author", "AUTHOR"]}}]`

func (c *Catalog) langFilter() string {
	return fmt.Sprintf(`_or: [{language_id: {_is_null: true}}, {language: {code2: {_eq: %q}}}]`, c.Lang.Code2)
}

func (c *Catalog) worksQuery() string {
	lang := c.langFilter()
	comp := ""
	if !c.IncludeCompilations {
		comp = "compilation: {_eq: false},"
	}
	edFields := `id title asin isbn_13 isbn_10 edition_format release_date pages audio_seconds users_count
		publisher { name } language { language } contributions { contribution author { name } }`
	return `query AuthorWorks($authorId: Int!, $limit: Int!, $offset: Int!, $edLimit: Int!) {
		books(
			where: {
				canonical_id: {_is_null: true}, ` + comp + `
				contributions: {author_id: {_eq: $authorId}, ` + authorRoleFilter + `},
				editions: {` + lang + `}
			},
			order_by: [{users_count: desc}, {id: asc}], limit: $limit, offset: $offset
		) {
			id title subtitle slug release_year users_count compilation image { url }
			book_series(order_by: {position: asc}) { position featured series { name } }
			audio: editions(where: {reading_format_id: {_eq: 2}, ` + lang + `}, order_by: [{users_count: desc}, {id: asc}], limit: $edLimit) { ` + edFields + ` }
			ebook: editions(where: {reading_format_id: {_eq: 4}, ` + lang + `}, order_by: [{users_count: desc}, {id: asc}], limit: $edLimit) { ` + edFields + ` }
			audio_ids: editions(where: {reading_format_id: {_eq: 2}, ` + lang + `}) { id }
			ebook_ids: editions(where: {reading_format_id: {_eq: 4}, ` + lang + `}) { id }
			print_ids: editions(where: {reading_format_id: {_eq: 1}, ` + lang + `}) { id }
		}
	}`
}

// BuildAuthor reads an author and every work of theirs with an edition in
// the configured language (or none recorded) on Hardcover.
func (c *Catalog) BuildAuthor(ctx context.Context, id int) (*Author, error) {
	var (
		row   *hcAuthor
		books []hcBook
		errs  [2]error
		wg    sync.WaitGroup
	)
	// A missing or placeholder author stops the works fetch instead of
	// paging through thousands of anthologies.
	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wg.Add(2)
	go func() {
		defer wg.Done()
		if row, errs[0] = c.authorRow(fetchCtx, id); errs[0] != nil {
			cancel()
		}
	}()
	go func() { defer wg.Done(); books, errs[1] = c.works(fetchCtx, id) }()
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	a := &Author{ID: row.ID, Name: row.Name, Slug: row.Slug, Bio: row.Bio, Books: make([]Book, 0, len(books)), Built: time.Now()}
	if row.Image != nil {
		a.ImageURL = row.Image.URL
	}
	for _, hb := range books {
		a.Books = append(a.Books, c.mapBook(hb))
	}
	return a, nil
}

func (c *Catalog) authorRow(ctx context.Context, id int) (*hcAuthor, error) {
	var resp struct {
		Data struct {
			Authors []hcAuthor `json:"authors"`
		} `json:"data"`
	}
	q := `query Author($id: Int!) { authors(where: {id: {_eq: $id}}, limit: 1) { id name slug bio image { url } } }`
	if err := c.HC.Query(ctx, q, map[string]any{"id": id}, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data.Authors) == 0 || IsPlaceholderAuthor(resp.Data.Authors[0].Name) {
		return nil, ErrNotFound
	}
	return &resp.Data.Authors[0], nil
}

func (c *Catalog) works(ctx context.Context, authorID int) ([]hcBook, error) {
	const pageSize = 100
	q := c.worksQuery()
	var all []hcBook
	for offset := 0; offset < 5000; offset += pageSize {
		var resp struct {
			Data struct {
				Books []hcBook `json:"books"`
			} `json:"data"`
		}
		vars := map[string]any{"authorId": authorID, "limit": pageSize, "offset": offset, "edLimit": editionDetail}
		if err := c.HC.Query(ctx, q, vars, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Data.Books...)
		if len(resp.Data.Books) < pageSize {
			break
		}
	}
	return all, nil
}

func (c *Catalog) mapBook(hb hcBook) Book {
	b := Book{ID: hb.ID, Title: hb.Title, Subtitle: hb.Subtitle, Slug: hb.Slug, Year: hb.ReleaseYear,
		Readers: hb.UsersCount, Compilation: hb.Compilation, HasPrint: len(hb.PrintIDs) > 0,
		EbookCount: len(hb.EbookIDs), AudioCount: len(hb.AudioIDs), PrintCount: len(hb.PrintIDs),
		Ebook: []Edition{}, Audio: []Edition{}}
	if hb.Image != nil {
		b.CoverURL = hb.Image.URL
	}
	for i, s := range hb.BookSeries {
		if s.Featured || i == 0 {
			b.Series = s.Series.Name
			b.SeriesPos = ""
			if s.Position != nil {
				b.SeriesPos = strconv.FormatFloat(*s.Position, 'f', -1, 64)
			}
			if s.Featured {
				break
			}
		}
	}
	for _, e := range hb.Ebook {
		b.Ebook = append(b.Ebook, c.mapEdition(e, FormatEbook, hb.Title))
	}
	for _, e := range hb.Audio {
		b.Audio = append(b.Audio, c.mapEdition(e, FormatAudio, hb.Title))
	}
	return b
}

func (c *Catalog) mapEdition(e hcEdition, format, bookTitle string) Edition {
	ed := Edition{Source: "hardcover", ID: strconv.Itoa(e.ID), Format: format, EditionFormat: e.EditionFormat,
		Title: e.Title, Date: e.ReleaseDate, ISBN13: e.Isbn13, ISBN10: e.Isbn10, ASIN: e.Asin,
		Pages: e.Pages, DurationMin: e.AudioSeconds / 60, Readers: e.UsersCount}
	if ed.Title == "" {
		ed.Title = bookTitle
	}
	if e.Publisher != nil {
		ed.Publisher = e.Publisher.Name
	}
	if e.Language != nil {
		ed.Language = e.Language.Language
	}
	for _, ct := range e.Contributions {
		if ct.Contribution != nil && strings.EqualFold(strings.TrimSpace(*ct.Contribution), "Narrator") {
			ed.Narrators = append(ed.Narrators, ct.Author.Name)
		}
	}
	return ed
}

// SortBooks orders books by series then position, then by title; books
// outside a series come after.
func SortBooks(books []Book) {
	sort.SliceStable(books, func(i, j int) bool {
		a, b := books[i], books[j]
		if (a.Series == "") != (b.Series == "") {
			return a.Series != ""
		}
		if a.Series != b.Series {
			return strings.ToLower(a.Series) < strings.ToLower(b.Series)
		}
		pa, ea := strconv.ParseFloat(a.SeriesPos, 64)
		pb, eb := strconv.ParseFloat(b.SeriesPos, 64)
		if ea == nil && eb == nil && pa != pb {
			return pa < pb
		}
		return strings.ToLower(a.Title) < strings.ToLower(b.Title)
	})
}
