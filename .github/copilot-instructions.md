# audio-scout — Copilot Instructions

This document gives AI coding agents full context about the project so they can contribute accurately without re-deriving things from scratch.

---

## Project purpose

`audio-scout` is a single-binary CLI tool (Go) that checks [Libby / OverDrive](https://www.overdrive.com/) library catalogs for audiobook availability. It has three operating modes:

| Mode | Flag(s) | What it does |
|---|---|---|
| **Single-book** | `--title` / `--author` | Checks one book across all specified libraries |
| **Goodreads to-read** | `--goodreads` | Checks every book on the user's Goodreads "to-read" shelf |
| **Series next-up** | `--goodreads --series` | Finds every in-progress series, determines the next unread book, checks Libby |

An optional **Audible mode** (`--audible`) inverts the filter — it outputs books that are *not* available or reservable anywhere, so the user can prioritise Audible credit purchases by page count (fetched from Google Books / Open Library).

---

## Tech stack

| Layer | Choice |
|---|---|
| Language | **Go 1.25** (stdlib-only — no third-party dependencies; `go.sum` is empty) |
| HTTP | `net/http` with a custom token-bucket rate limiter and retry/backoff |
| Concurrency | `sync.WaitGroup`, `sync/atomic`, semaphore channels, goroutine worker pools |
| External APIs | [Thunder API](https://thunder.api.overdrive.com) (OverDrive/Libby), Google Books API, Open Library API, Goodreads HTML scraping |
| Output formats | Plain columnar text (default), CSV (`--csv`), JSON (`--json`) |
| Lint / static analysis | `golangci-lint` (with `--fix`), `staticcheck` |
| CI | GitHub Actions (`.github/workflows/ci.yml`) — runs on push/PR to `main` |
| Build | `make` (GNU Make) |
| Git hooks | `.githooks/pre-commit` — activated by `make init` |

---

## Repository layout

```
audio-scout/
├── main.go                  # entire application — single package main, ~1270 lines
├── go.mod                   # module: audio-scout, go 1.25, no external deps
├── go.sum                   # empty (no deps)
├── Makefile                 # build / check / init targets
├── README.md                # user-facing documentation
├── testdata/
│   └── goodreads_library_export.csv   # real Goodreads export used for manual testing
├── bin/                     # compiled binary (git-ignored)
│   └── audio-scout
├── .githooks/
│   └── pre-commit           # staticcheck → lint --fix → tests; blocks direct commits to main
├── .github/
│   ├── workflows/
│   │   └── ci.yml           # GitHub Actions CI pipeline
│   └── copilot-instructions.md   # this file
├── .gitignore               # ignores bin/, run-*, *.log, .env*, IDE dirs, /audio-scout root binary
└── run/                     # git-ignored; holds run-*.txt / run-*.csv output files
```

> **Note:** The project is intentionally kept as a single `main` package. The codebase is ~1270 lines and the authors have explicitly decided *not* to split into sub-packages at this size.

---

## Key types (all in `main.go`)

```go
// Library — a Thunder API key + display name pair
type Library struct { Name, Key string }

// BookQuery — what we're searching for; passed into checkLibby
type BookQuery struct {
    Title      string
    Author     string
    DaysOnList int    // 0 in single-book mode; populated from Goodreads "Date Added"
    SeriesNote string // "in your Goodreads" | "not in your Goodreads" | "up next — currently reading #N"
    SeriesName string // populated in series mode
}

// availResponse — parsed from the Thunder availability endpoint
type availResponse struct {
    IsAvailable     bool
    AvailableCopies int
    OwnedCopies     int
    HoldsCount      int
    DurationMinutes int  // derived from mediaDetail.Formats[*].Duration ("HH:MM:SS")
}

// result — per-library result, used in single-book mode and JSON output
type result struct { Library, Found, Available, Owned, AvailableCopies, Holds, DurationMinutes, Error }

// bookResult — pairs a BookQuery with a per-library result (used in Goodreads mode)
type bookResult struct { Book BookQuery; Library string; result }

// goodreadsRow — one row from the Goodreads CSV that made it through filtering
type goodreadsRow struct { Title, Author string; DaysOnList int; SeriesNote, SeriesName string }

// seriesEntry — one book in a series as found in the Goodreads CSV
type seriesEntry struct { GoodreadsID, Title, Author, Shelf string; Num float64 }

// mediaDetail — partial parse of the Thunder media detail endpoint
type mediaDetail struct { MediaType string; Formats []{ ID, Duration string } }
```

---

## Core functions

| Function | Purpose |
|---|---|
| `newRateLimiter(rps int)` | Token-bucket rate limiter; returns a `<-chan struct{}` and a stop channel |
| `doGetWithCtx(...)` | GET with context, rate-limit token consumption, 3-attempt retry with backoff; **do NOT use `defer resp.Body.Close()`** inside this function — close is handled explicitly via a closure to avoid holding bodies open across retries |
| `getJSONWithCtx(...)` | Thin wrapper: `doGetWithCtx` + `json.Unmarshal` |
| `lookupPageCount(...)` | Google Books → Open Library fallback; returns page count integer or 0 |
| `checkLibby(...)` | Search Thunder catalog → find audiobook mediaType → fetch availability; returns `availResponse` |
| `parseGoodreadsToRead(path)` | Reads Goodreads CSV; two-pass: first pass collects "read" titles to deduplicate, second pass returns only "to-read" books not already read |
| `parseSeriesNextBooks(ctx, path, verbose, skipNovellas)` | Reads full Goodreads CSV; resolves next-in-series books; uses a 4-worker pool to fan out Goodreads HTML scraping |
| `goodreadsSeriesPage(goodreadsID)` | Scrapes Goodreads book page to find series slug, then fetches series page and regex-extracts `(position, bare title)` pairs from embedded JSON |
| `runGoodreadsMode(...)` | Main dispatch loop: semaphore-bounded goroutine fan-out across books × libraries; collects results; handles both normal and Audible output modes |
| `truncate(s, n)` | Rune-aware string truncation with `…` suffix |
| `formatDuration(minutes)` | Formats minutes as `"12h 34m"` |
| `parseDurationMinutes(s)` | Parses `"HH:MM:SS"` → integer minutes |

---

## Concurrency model

1. **Rate limiter** — a single shared token-bucket channel (`newRateLimiter`) is passed to all goroutines. All HTTP requests (Thunder, Google Books, Open Library, Goodreads scraping) block on it before firing.
2. **Semaphore channel** — `sem := make(chan struct{}, parallel)` caps the number of in-flight goroutines. Default `--parallel 8`.
3. **Worker pools** — the series lookup phase uses a fixed pool of 4 workers (`seriesWorkers = 4`), not the user-controlled `--parallel` value.
4. **Result channels** — `resCh` and `missCh` are buffered with `total * len(libraries)` capacity so goroutines never block writing results. Both are closed after `wg.Wait()` before ranging over them.
5. **`sync.WaitGroup` + `atomic.Int64`** — `wg` tracks goroutine completion; `completed` provides a lock-free progress counter.

---

## HTTP / API conventions

- **User-Agent** is always set to a real Chrome UA string. The Thunder API and Goodreads both block requests without one.
- **Thunder API base:** `https://thunder.api.overdrive.com/v2/libraries/{key}/`
  - Search: `.../media?query={q}&perPage=20`
  - Detail: `.../media/{id}`
  - Availability: `.../media/{id}/availability`
- **Goodreads scraping** (no API key needed):
  - Book page: `https://www.goodreads.com/book/show/{goodreadsID}`
  - Series page: `https://www.goodreads.com/series/{slug}`
  - Extracted via regex from embedded JSON in HTML (`seriesSlugRE`, `seriesBookRE`)
- **Google Books:** `https://www.googleapis.com/books/v1/volumes?q=intitle:{t}+inauthor:{a}&maxResults=3&fields=items/volumeInfo/pageCount`
- **Open Library:** `https://openlibrary.org/search.json?title={t}&fields=number_of_pages_median&limit=1`
- Retry: 3 attempts, exponential backoff (300ms, 600ms, 900ms for generic errors; 5s, 10s, 15s for HTTP 429).
- `resp.Body.Close()` errors are always checked and logged — never silently ignored. Never use `defer resp.Body.Close()` inside a retry loop.

---

## Goodreads CSV format

The Goodreads export CSV has these columns (among others) that the app uses:

| Column | Use |
|---|---|
| `Title` | Raw title, may contain series suffix like `(Kingkiller Chronicle, #1)` |
| `Author` | "First Last" format |
| `Exclusive Shelf` | `"read"`, `"to-read"`, `"currently-reading"` |
| `Date Added` | Format `2006/01/02`; used to calculate `DaysOnList` |
| `Book Id` | Goodreads numeric ID; used to fetch the book page for series lookup |

Series info is parsed from the title with `seriesRE`:
```
\(([^)]+),\s*#([0-9]+(?:\.[0-9]+)?)\)\s*$
```
A book is a "novella" (skippable with `--skip-novellas`) if its series position is non-integer (e.g. `#1.5`).

---

## Output format

### Normal / series mode (plain text)
```
AVAILABLE  %-55s  %-35s  %-9s  %-30s  %-10s  %s
           Title   SeriesName  "to-read"  Author  Duration  Libraries
```
- `AVAILABLE` vs `WAITLIST ` (note trailing space — 9 chars each for alignment)
- Duration: `"12h 34m"` — blank if unknown
- `to-read` column: `"to-read"` if book is in the user's Goodreads list, blank if discovered via series lookup
- Libraries: comma-joined, e.g. `pittsburgh,chester,freelibrary`

### CSV mode (`--csv`)
Headers: `Status,Title,Series,In Goodreads,Author,Duration,Minutes,Libraries`

### Audible mode (plain text)
```
%-55s  %-35s  %-30s  %s
Title   SeriesName  Author   "NNN pages"
```

### Audible CSV (`--audible --csv`)
Headers: `Title,Series,Author,Pages`

### JSON mode (`--json`)
Emits a JSON array of `bookSummary` structs (normal mode) or `result` structs (single-book mode).

---

## Build & development workflow

```bash
# One-time setup after cloning:
make init          # sets git core.hooksPath to .githooks/; creates .git/hooks/.githooks-installed sentinel

# Development loop:
make checks        # staticcheck → golangci-lint --fix → go test ./...
make build         # runs checks, then: go build -o bin/audio-scout main.go
make clean         # rm -rf bin/

# The sentinel file .git/hooks/.githooks-installed must exist or make checks/build abort.
# This ensures every contributor runs make init first.
```

### Pre-commit hook (`.githooks/pre-commit`)
1. Blocks direct commits to `main` — always work on a branch.
2. Runs `make staticcheck` — fails commit on any issue.
3. Runs `make lint` (`golangci-lint --fix`) — if it modifies files, aborts and asks developer to review + stage.
4. Runs `make test` — fails commit if tests fail.

### CI (`.github/workflows/ci.yml`)
Runs on every push/PR to `main`: `make init` → `make checks` → `make test` → `make build`.

---

## Coding standards

- **No external dependencies.** The module is stdlib-only. Do not add `go get` dependencies.
- **Error handling:** always check errors, always wrap with `fmt.Errorf("context: %w", err)`. Never swallow errors silently.
- **`resp.Body.Close()`** must be checked. Use an explicit closure (`closeBody := func() { if cerr := resp.Body.Close(); cerr != nil { log.Printf(...) } }`) inside retry loops instead of `defer`.
- **`defer f.Close()`** for file handles uses the same pattern: wrap in an anonymous func to check the error.
- **`fmt.Fprintf(os.Stderr, ...)`** return values must be captured: `_, _ = fmt.Fprintf(os.Stderr, ...)`.
- **Progress/warnings → stderr, results → stdout.** This is a hard rule throughout the codebase.
- **Unused fields/variables** must be removed (staticcheck U1000 is enforced).
- **`defer` inside loops** is not permitted (GoLang inspection warning; use explicit close closures instead).
- Column widths in `fmt.Printf` format strings use fixed `%-Ns` padding — keep alignment consistent with existing output.
- Use `strings.ToLower` for all case-insensitive key comparisons.
- `truncate(s, n)` is rune-aware — always use it when trimming display strings.
- Series position numbers are `float64` throughout (to support novellas like `#1.5`).
- The deduplication pattern (`map[string]struct{}`) is used in several places — follow the same pattern for any new deduplication needs.

---

## Run output files

Results go to files in a run directory which is git-ignored. Typical usage:

```bash
# to-read list
time ./bin/audio-scout \
  --goodreads testdata/goodreads_library_export.csv \
  --libs pittsburgh,freelibrary,chester \
  --rate 20 --parallel 8 --timeout 15 --verbose \
  > run/available.txt \
  2> >(tee run/errors.txt)

# series next-up
time ./bin/audio-scout \
  --goodreads testdata/goodreads_library_export.csv \
  --series \
  --libs pittsburgh,freelibrary,chester \
  --verbose \
  > run/next-up.txt \
  2> >(tee run/errors.txt)

# audible credit planning
time ./bin/audio-scout \
  --goodreads testdata/goodreads_library_export.csv \
  --libs pittsburgh,freelibrary,chester \
  --audible --csv --verbose \
  > run/audible.csv \
  2> >(tee run/errors.txt)
```

Typical runtimes (750-book Goodreads export, 3 libraries, `--rate 20`): ~3–5 minutes for to-read mode, ~6–10 minutes for series mode.

---

## Known limitations / design decisions

- **No Hoopla support.** Hoopla's search backend (`search-api.hoopladigital.com/prod/`) requires AWS Signature V4 auth. There is no public API.
- **Goodreads scraping** is rate-limited by their server (~15s timeouts are common for popular series pages). The retry logic (up to 3 attempts, exponential backoff at 5s/20s/45s) handles this. Expect 5–10 timeout-retries per full series run.
- **Series position collisions** across multi-named series (e.g. `"George Smiley"` and `"George Smiley, #6; Karla Trilogy"`) are handled by the `readTitles` global set — a title already read under any series name is skipped.
- **Deduplication in `runGoodreadsMode`** — when series mode produces the same next-book title under two different series names, the second occurrence is dropped via `seenTitles`.
- The app is intentionally a **single `main` package** — no sub-packages. Maintainers have explicitly decided not to split at the current scale (~1270 lines).

