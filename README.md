# edition-scout

A small self-hosted web app for [Hardcover](https://hardcover.app) librarians and readers. Search for an author and edition-scout shows their Hardcover catalogue with the ebook, audiobook and print editions each book has. In the background it finds the editions Hardcover is **missing**: ebooks and audiobooks that exist, with the ISBN or ASIN, publisher, date and narrator you need to add them on Hardcover.

- **Author search** over Hardcover, or a numeric Hardcover author id.
- **Catalogue view** for each book, with a status for each format: *On Hardcover* (with how many editions it has), *Missing* (found elsewhere, with a link), *None found*, or *Checking…*.
- **Missing on Hardcover report** across all your authors, with CSV and JSON export.
- **Background checks:** authors you open stay on your list and are re-read from Hardcover daily. When Hardcover gains an edition, the gap closes and the report says so.
- One ~12 MB Docker image, no database, and state kept in a few JSON files.

## Quick start

```sh
cp .env.example .env        # add your HARDCOVER_API_TOKEN
docker compose up -d --build
```

Open http://localhost:8080 and search for an author.

You can get a Hardcover API token at hardcover.app → Settings → API. Hardcover allows about 60 requests a minute per token. edition-scout stays under that, so a large catalogue (200+ books) can take a minute to load the first time.

## Where editions come from

When Hardcover has no ebook or no audiobook edition of a book, edition-scout asks these sources in order and stops at the first match:

| Format | Sources | Notes |
|---|---|---|
| Audiobook | **Audible** catalogue, then Goodreads (if enabled) | Audible's public catalogue API (no key). Gives narrators, runtime and ASIN. Region via `SCOUT_AUDIBLE_REGION`. |
| Ebook | Goodreads (if enabled), then **Google Books**, then **Open Library** | Google Books needs a free API key (1,000 requests a day). Open Library only counts editions explicitly labelled as ebooks. |

Matching is strict. The title must agree, allowing a series prefix or subtitle but not a different instalment ("Season 2", "Book 3"). The author must be credited, and the edition must be in your language. A miss is fine; a wrong edition isn't.

Some books aren't looked up, and the catalogue shows the reason:
- books that only have print editions and fewer than `SCOUT_PRINT_ONLY_MIN_READERS` Hardcover readers;
- records that are a split part or duplicate of a book that already has the format ("The Way of Kings, Part 2");
- "Untitled" placeholders.

Lookups run in a background queue, never on page load. Each source is paced. When a source is paused (Google's daily quota is used up, or Goodreads shows a bot check), lookups that still need it wait until it's back. They aren't recorded as "none found" in the meantime. Results are cached: 30 days for a find, 14 days for a miss.

### Goodreads (opt-in)

Goodreads has had no public API since 2020. With `SCOUT_GOODREADS=true`, edition-scout reads two kinds of public Goodreads pages: the search autocomplete and a work's editions list. It requests one page every 4 seconds and backs off for 30 minutes if Goodreads shows a bot check. Check that this fits your use before you turn it on.

## Configuration

| Variable | Default | |
|---|---|---|
| `HARDCOVER_API_TOKEN` | — | **Required.** |
| `GOOGLE_BOOKS_API_KEY` | — | Recommended: enables Google Books for ebooks. |
| `SCOUT_AUDIBLE_REGION` | `us` | `us uk ca au de fr it es in jp`, or `off`. |
| `SCOUT_GOODREADS` | `false` | Opt-in Goodreads lookups (see above). |
| `SCOUT_OPENLIBRARY` | `true` | Open Library for ebooks. |
| `SCOUT_LANGUAGE` | `en` | Edition language: `en de fr es it nl pt`. |
| `SCOUT_REFRESH_INTERVAL` | `24h` | How often followed authors are re-read from Hardcover. |
| `SCOUT_PRINT_ONLY_MIN_READERS` | `5` | Print-only books are looked up from this many readers. |
| `SCOUT_INCLUDE_COMPILATIONS` | `true` | `false` hides boxed sets and omnibuses. |
| `SCOUT_BLOCKED_AUTHORS` | — | Extra placeholder names to hide, comma-separated. "Various", "Unknown", "Misc" and similar are built in. |
| `SCOUT_DATA_DIR` | `/data` | Where state is kept. |
| `SCOUT_ADDR` | `:8080` | Listen address. |
| `SCOUT_DEBUG` | — | `true` for debug logs. |

## JSON API

| Endpoint | |
|---|---|
| `GET /api/search?q=` | Hardcover author search. |
| `GET /api/authors` | Your authors with counts. |
| `GET /api/authors/{id}` | Catalogue with each format's status. Adds the author to your list. Returns `202` while a first load is running; `?refresh=1` re-reads Hardcover. |
| `GET /api/report` | Everything missing on Hardcover, plus gaps closed in the last 14 days. |
| `GET /api/status` | Queue sizes, paused sources and Hardcover request stats. |

`GET /report?format=csv` downloads the report as CSV.

## Data

`/data` holds `tracked.json` (your authors), `authors/<id>.json` (last catalogue read), and `gaps*.json` (lookup results, queue, recently closed gaps). Delete the directory to start over.

## Building without Docker

```sh
go build -o edition-scout ./cmd/edition-scout
HARDCOVER_API_TOKEN=... SCOUT_DATA_DIR=./data ./edition-scout
```

## Credits

The title matching and gap-finding logic started in a fork of [Bindery](https://github.com/vavallee/bindery) (MIT). Book data comes from Hardcover, Audible, Google Books, Open Library and (optionally) Goodreads. edition-scout isn't affiliated with any of them.

Licensed under MIT (see `LICENSE`). `THIRD_PARTY_NOTICES` has Bindery's MIT notice and the BSD-3-Clause notice for the Go standard library and `golang.org/x/text`, which are built into the binaries. The notices are also at `/licenses` in the image.
