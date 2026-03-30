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
./bin/audio-scout \
  --goodreads goodreads_library_export.csv \
  --libs pittsburgh,chester,freelibrary
```

Only books on your **to-read** shelf are checked. Books with no audiobook edition, or where the library doesn't own a copy, produce no output.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--title` | `"Lore"` | Book title to search for (single-book mode) |
| `--author` | `"Alexandra Bracken"` | Author name (single-book mode, optional) |
| `--libs` | `pittsburgh,chester,freelibrary` | Comma-separated library keys |
| `--goodreads` | _(none)_ | Path to a Goodreads CSV export; checks all to-read books |
| `--rate` | `5` | Max HTTP requests per second toward the Thunder API |
| `--parallel` | `4` | Number of concurrent worker goroutines |
| `--timeout` | `5` | Per-request HTTP timeout in seconds |
| `--json` | `false` | Emit results as JSON instead of plain text |
| `--verbose` | `false` | Print progress and hits to stderr while running |

## Output

By default, output is plain columnar text — one line per hit — designed to be piped:

```
  Fahrenheit 451                                      pittsburgh     AVAILABLE NOW       (owned: 3, available: 1, holds: 2)
  The Nightingale                                     freelibrary    AVAILABLE NOW       (owned: 5, available: 2, holds: 0)
  Watership Down                                      chester        Reserve (waitlist)  (owned: 2, available: 0, holds: 7)
```

The tool is silent if there are no results — no output at all, exit 0.

### Useful pipes

```bash
# Only books available right now
./bin/audio-scout --goodreads export.csv --libs pittsburgh | grep "AVAILABLE NOW"

# Sort by library
./bin/audio-scout --goodreads export.csv --libs pittsburgh,chester | sort -k2

# Count hits per library
./bin/audio-scout --goodreads export.csv --libs pittsburgh,chester | awk '{print $2}' | sort | uniq -c

# JSON output piped to jq
./bin/audio-scout --goodreads export.csv --json | jq '.[] | select(.available == true) | .Book.Title'

# Save results to a file; hits and progress stream to stderr so you can watch
./bin/audio-scout --goodreads export.csv --verbose > results.txt
```

## Rate limiting

The Thunder API is public and unauthenticated. The default `--rate 5` (5 requests/second) is conservative and polite. A full Goodreads to-read list of ~750 books across 3 libraries takes roughly **30–50 minutes** at this rate.

If you see HTTP 429 responses, lower the rate:

```bash
./bin/audio-scout --goodreads export.csv --rate 3
```

## Development

```bash
make checks   # staticcheck + golangci-lint + tests
make build    # runs checks, then compiles
make clean    # remove bin/
```

## License

MIT

