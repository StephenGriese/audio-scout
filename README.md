# audio-scout

A command-line tool that checks [Libby / OverDrive](https://www.overdrive.com/) library catalogs for audiobook availability. Given a book title and author — or an entire Goodreads to-read list — it tells you which of your libraries has the audiobook available to borrow right now, or available to place on hold.

## How it works

audio-scout uses the public [Thunder API](https://thunder.api.overdrive.com) that powers the Libby app. For each book it:

1. Searches the library's catalog by title + author
2. Inspects the top results to find an audiobook edition
3. Checks availability (copies owned, copies available, hold queue depth)
4. Reports only books that are **available now** or **reservable** (owned but checked out) — everything else is silently skipped

## Installation

Requires [Go 1.21+](https://go.dev/dl/).

```bash
git clone https://github.com/youruser/audio-scout.git
cd audio-scout
make build
```

The binary is placed at `bin/audio-scout`.

## Finding your library's key

Use the Thunder API's library search endpoint:

```bash
curl "https://thunder.api.overdrive.com/v2/libraries?query=pittsburgh&perPage=5" | jq '.[].items[] | {id, name}'
```

The `id` (or `preferredKey`) field is what you pass to `--libs`. For example, the Carnegie Library of Pittsburgh is `pittsburgh`, the Free Library of Philadelphia is `freelibrary`.

Alternatively, open [libbyapp.com](https://libbyapp.com) in your browser, sign in, and look at any request in the Network tab — requests to `thunder.api.overdrive.com/v2/libraries/{key}/...` will show your library's key.

## Usage

### Check a single book

```bash
./bin/audio-scout \
  --title "Watership Down" \
  --author "Richard Adams" \
  --libs pittsburgh,chester,freelibrary
```

### Check your entire Goodreads to-read list

Export your library from Goodreads (**My Books → Import and Export → Export Library**), then:

```bash
time ./bin/audio-scout \
  --goodreads testdata/goodreads_library_export.csv \
  --libs pittsburgh,chester,freelibrary \
  --rate 20 \
  --parallel 8 \
  --timeout 15 \
  --verbose \
  > available.txt
```

Only books on your **to-read** shelf are checked. Books with no audiobook edition, or where the library doesn't own a copy, produce no output. Progress and any warnings stream to stderr so you can watch the run while stdout goes cleanly to the file. Prepending `time` gives you a wall-clock summary when it finishes.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--title` | `"Lore"` | Book title to search for (single-book mode) |
| `--author` | `"Alexandra Bracken"` | Author name (single-book mode, optional) |
| `--libs` | `pittsburgh,chester,freelibrary` | Comma-separated library keys |
| `--goodreads` | _(none)_ | Path to a Goodreads CSV export; checks all to-read books |
| `--rate` | `20` | Max HTTP requests per second toward the Thunder API |
| `--parallel` | `8` | Number of concurrent worker goroutines |
| `--timeout` | `15` | Per-request HTTP timeout in seconds |
| `--json` | `false` | Emit results as JSON instead of plain text |
| `--verbose` | `false` | Print progress and hits to stderr while running |

## Output

By default, output is plain columnar text — one line per book — designed to be piped:

```
AVAILABLE  Dinner for Vampires                          Wil Wheaton                      312d  pittsburgh,freelibrary
AVAILABLE  The Nightingale                              Kristin Hannah                   891d  chester
WAITLIST   1929: Inside the Greatest Crash in History…  Lionel Laurent                  1204d  pittsburgh,chester,freelibrary
WAITLIST   Watership Down                               Richard Adams                     47d  chester
```

- **`AVAILABLE`** — at least one library has a copy ready to borrow right now
- **`WAITLIST`** — every library that owns it has all copies checked out; you can place a hold
- **`312d`** — days the book has been on your to-read list (from Goodreads `Date Added`); useful for prioritising long-neglected titles
- Libraries are collapsed into a single comma-separated column per book — if multiple libraries have it, they all appear on one line
- Results are sorted: `AVAILABLE` first, then `WAITLIST`

The tool is silent if there are no results — no output at all, exit 0.

### stderr vs stdout

Progress output (`--verbose`) and error messages go to **stderr**. Results go to **stdout**. These are separate streams, so you can redirect results to a file while still watching progress in your terminal:

```bash
./bin/audio-scout --goodreads export.csv --verbose > results.txt
# stderr (progress) prints to your terminal
# stdout (results) goes to results.txt
```

This means `--verbose` never pollutes a pipe or redirect — it's safe to always use it for long-running scans.

### Useful pipes

```bash
# Only books available right now
./bin/audio-scout --goodreads export.csv --libs pittsburgh | grep "^AVAILABLE"

# Find books by a favourite author
./bin/audio-scout --goodreads export.csv --libs pittsburgh | grep "Le Guin"

# Sort by how long the book has been on your list (longest wait first)
# Column 4 is the days field (numeric, e.g. "1204d") — strip the trailing 'd' for pure numeric sort
./bin/audio-scout --goodreads export.csv --libs pittsburgh,chester | sort -k4 -rn

# Sort alphabetically by title within each status group (already the default)
./bin/audio-scout --goodreads export.csv --libs pittsburgh,chester | sort -k2

# Count hits per status
./bin/audio-scout --goodreads export.csv --libs pittsburgh,chester | awk '{print $1}' | sort | uniq -c

# JSON output piped to jq
./bin/audio-scout --goodreads export.csv --json | jq '.[] | select(.Available) | .Title'

# Save results to a file; hits and progress stream to stderr so you can watch
./bin/audio-scout --goodreads export.csv --verbose > results.txt
```

## Rate limiting

The Thunder API is public and unauthenticated. The default `--rate 5` (5 requests/second) is conservative and polite. A full Goodreads to-read list of ~750 books across 3 libraries takes roughly **3–4 minutes** at this rate.

If the Thunder API rate-limits you, a warning is printed to **stderr**:

```
warning: rate-limited (HTTP 429) by Thunder API — consider lowering --rate (attempt 1)
```

The request will be retried automatically with backoff, but if you see many of these, lower the rate:

```bash
./bin/audio-scout --goodreads export.csv --rate 3
```

## Development

```bash
make init     # FIRST: configures git to use .githooks/ (required once per clone)
make checks   # staticcheck + golangci-lint + tests
make build    # runs checks, then compiles
make clean    # remove bin/
```

> `make init` only needs to be run once after cloning. It points git at the project's `.githooks/` directory so pre-commit checks run automatically. `make checks` and `make build` will refuse to run until it has been called.

## License

MIT - Normal for hobby projects

