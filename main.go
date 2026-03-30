package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Library holds the display name and the Thunder API key for the library.
// Find a library's key at: https://thunder.api.overdrive.com/v2/libraries/{key}
type Library struct {
	Name string
	Key  string
}

// BookQuery is the title + optional author used to search for a specific audiobook.
type BookQuery struct {
	Title       string
	Author      string
	DaysOnList  int // 0 when unknown (e.g. single-book mode)
}

func (b BookQuery) searchQuery() string {
	q := b.Title
	if b.Author != "" {
		q += " " + b.Author
	}
	return url.QueryEscape(q)
}

type searchResponse struct {
	Items []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"items"`
}

type availResponse struct {
	IsAvailable     bool `json:"isAvailable"`
	AvailableCopies int  `json:"availableCopies"`
	OwnedCopies     int  `json:"ownedCopies"`
	HoldsCount      int  `json:"holdsCount"`
}

// result is a structured result we can print or JSON encode.
type result struct {
	Library         string `json:"library"`
	Found           bool   `json:"found"`
	Available       bool   `json:"available"`
	Owned           int    `json:"ownedCopies"`
	AvailableCopies int    `json:"availableCopies"`
	Holds           int    `json:"holdsCount"`
	Error           string `json:"error,omitempty"`
}

// newRateLimiter returns a channel that emits a token every (1/rps) duration.
// Callers block on receiving from it before making an HTTP request.
// Close the returned stop channel to shut down the background goroutine.
func newRateLimiter(rps int) (tokens <-chan struct{}, stop chan struct{}) {
	ch := make(chan struct{}, rps) // small buffer so bursts don't stall
	stopCh := make(chan struct{})
	interval := time.Second / time.Duration(rps)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				select {
				case ch <- struct{}{}:
				default: // bucket full, drop token
				}
			}
		}
	}()
	return ch, stopCh
}

// doGetWithCtx performs a GET with context, rate-limiting, and simple retries/backoff.
func doGetWithCtx(ctx context.Context, client *http.Client, limiter <-chan struct{}, u string) ([]byte, error) {
	var lastErr error
	// simple retry loop
	for attempt := 0; attempt < 3; attempt++ {
		// Wait for a rate-limit token before firing the request.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-limiter:
		}
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				// backoff
				time.Sleep(time.Duration(attempt+1) * 300 * time.Millisecond)
				continue
			}
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Printf("warning: failed to close response body: %v", err)
			}
		}()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				time.Sleep(time.Duration(attempt+1) * 300 * time.Millisecond)
				continue
			}
		}
		return b, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("unknown error")
	}
	return nil, lastErr
}

// getJSONWithCtx is a convenience wrapper to GET and unmarshal JSON with context/retries.
func getJSONWithCtx(ctx context.Context, client *http.Client, limiter <-chan struct{}, u string, out any) error {
	b, err := doGetWithCtx(ctx, client, limiter, u)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// mediaDetail contains minimal fields from the media detail endpoint we need to
// determine whether a result is an audiobook.
type mediaDetail struct {
	MediaType string `json:"mediaType"`
	Formats   []struct {
		ID string `json:"id"`
	} `json:"formats"`
}

// checkLibby returns the availability of the first audiobook result matching book at lib.
func checkLibby(ctx context.Context, client *http.Client, limiter <-chan struct{}, lib Library, book BookQuery) (availResponse, error) {
	var empty availResponse
	// Search for several results so we can prefer audiobook-format entries.
	searchURL := fmt.Sprintf("https://thunder.api.overdrive.com/v2/libraries/%s/media?query=%s&perPage=20", lib.Key, book.searchQuery())
	var sr searchResponse
	if err := getJSONWithCtx(ctx, client, limiter, searchURL, &sr); err != nil {
		return empty, fmt.Errorf("search request: %w", err)
	}
	if len(sr.Items) == 0 {
		return empty, fmt.Errorf("title %q not found in %s catalog", book.Title, lib.Name)
	}

	// Prefer an item that is an audiobook. We'll fetch each item's media detail and
	// choose the first where mediaType == "audiobook" or one of the formats contains "audiobook".
	// Cap at 5 detail lookups to avoid hammering the API for every search result.
	const maxDetailLookups = 5
	var titleID string
	for i, item := range sr.Items {
		if i >= maxDetailLookups {
			break
		}
		var md mediaDetail
		detailURL := fmt.Sprintf("https://thunder.api.overdrive.com/v2/libraries/%s/media/%s", lib.Key, item.ID)
		if err := getJSONWithCtx(ctx, client, limiter, detailURL, &md); err != nil {
			// ignore detail parse errors and try next
			continue
		}
		if strings.EqualFold(md.MediaType, "audiobook") {
			titleID = item.ID
			break
		}
		for _, f := range md.Formats {
			if strings.Contains(strings.ToLower(f.ID), "audiobook") {
				titleID = item.ID
				break
			}
		}
		if titleID != "" {
			break
		}
	}
	// If we didn't find an audiobook edition among the returned items, fail.
	if titleID == "" {
		return empty, fmt.Errorf("no audiobook edition found in %s catalog for %q", lib.Name, book.Title)
	}

	availURL := fmt.Sprintf("https://thunder.api.overdrive.com/v2/libraries/%s/media/%s/availability", lib.Key, titleID)
	body2, err := doGetWithCtx(ctx, client, limiter, availURL)
	if err != nil {
		return empty, fmt.Errorf("availability request: %w", err)
	}
	var av availResponse
	if err := json.Unmarshal(body2, &av); err != nil {
		return empty, fmt.Errorf("availability parse: %w", err)
	}
	return av, nil
}

// goodreadsRow holds the fields we care about from the Goodreads CSV export.
type goodreadsRow struct {
	Title       string
	Author      string // "First Last" format (column "Author")
	DaysOnList  int    // days since "Date Added" in the Goodreads export; 0 if unknown
}

// parseGoodreadsToRead reads a Goodreads library export CSV and returns all
// books whose "Exclusive Shelf" column equals "to-read".
func parseGoodreadsToRead(path string) ([]goodreadsRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			log.Printf("warning: failed to close goodreads file: %v", cerr)
		}
	}()

	r := csv.NewReader(f)
	r.LazyQuotes = true

	// Read header row to find column indices by name.
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	colIdx := make(map[string]int, len(header))
	for i, h := range header {
		colIdx[strings.TrimSpace(h)] = i
	}

	titleIdx, ok := colIdx["Title"]
	if !ok {
		return nil, fmt.Errorf("CSV missing 'Title' column")
	}
	authorIdx, ok := colIdx["Author"]
	if !ok {
		return nil, fmt.Errorf("CSV missing 'Author' column")
	}
	shelfIdx, ok := colIdx["Exclusive Shelf"]
	if !ok {
		return nil, fmt.Errorf("CSV missing 'Exclusive Shelf' column")
	}
	dateAddedIdx := colIdx["Date Added"] // optional; -1 if absent (map returns 0 for missing, but ok handles that)
	_, hasDateAdded := colIdx["Date Added"]

	today := time.Now().Truncate(24 * time.Hour)

	var books []goodreadsRow
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Skip malformed rows.
			continue
		}
		if len(rec) <= shelfIdx {
			continue
		}
		if strings.TrimSpace(rec[shelfIdx]) != "to-read" {
			continue
		}
		title := strings.TrimSpace(rec[titleIdx])
		author := strings.TrimSpace(rec[authorIdx])
		if title == "" {
			continue
		}
		var days int
		if hasDateAdded && dateAddedIdx < len(rec) {
			if raw := strings.TrimSpace(rec[dateAddedIdx]); raw != "" {
				if t, err := time.Parse("2006/01/02", raw); err == nil {
					days = int(today.Sub(t.Truncate(24 * time.Hour)).Hours() / 24)
				}
			}
		}
		books = append(books, goodreadsRow{Title: title, Author: author, DaysOnList: days})
	}
	return books, nil
}

// bookResult pairs a BookQuery with a per-library result.
type bookResult struct {
	Book    BookQuery
	Library string
	result
}

// runGoodreadsMode checks every to-read book across all libraries concurrently,
// printing only books that are available now or reservable (owned but no
// available copies — meaning you can place a hold).
func runGoodreadsMode(
	ctx context.Context,
	client *http.Client,
	limiter <-chan struct{},
	books []goodreadsRow,
	libraries []Library,
	parallel int,
	outJSON bool,
	verbose bool,
) {
	total := len(books)
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	var completed atomic.Int64
	resCh := make(chan bookResult, total*len(libraries))

	for _, row := range books {
		bq := BookQuery(row)
		for _, lib := range libraries {
			wg.Add(1)
			sem <- struct{}{}
			go func(b BookQuery, l Library) {
				defer wg.Done()
				defer func() { <-sem }()

				br := bookResult{Book: b, Library: l.Name}
				br.result.Library = l.Name
				av, err := checkLibby(ctx, client, limiter, l, b)
				if err != nil {
					// Not found or no audiobook edition — silently skip.
					return
				}
				// Only report if available NOW or reservable (library owns
				// copies but they're all checked out → holds queue open).
				reservable := av.OwnedCopies > 0 && !av.IsAvailable
				if !av.IsAvailable && !reservable {
					return
				}
				br.Found = true
				br.Available = av.IsAvailable
				br.Owned = av.OwnedCopies
				br.AvailableCopies = av.AvailableCopies
				br.Holds = av.HoldsCount
				resCh <- br
				if verbose {
					status := "WAITLIST"
					if br.Available {
						status = "AVAILABLE"
					}
					fmt.Fprintf(os.Stderr, "  [hit] %s @ %s — %s\n", truncate(b.Title, 50), l.Name, status)
				}
			}(bq, lib)
		}

		// Progress tick: once per book (after dispatching all library goroutines
		// for that book) so the counter reflects books dispatched, not completed.
		// We use a separate goroutine-safe counter on completion instead.
		n := completed.Add(1)
		if verbose && n%10 == 0 {
			fmt.Fprintf(os.Stderr, "  [progress] %d / %d books dispatched...\n", n, total)
		}
	}

	wg.Wait()
	close(resCh)

	// Collect hits and collapse per-library results into one row per book.
	// A book is AVAILABLE if any library has it now; otherwise WAITLIST.
	type bookSummary struct {
		Title       string
		Author      string
		Available   bool
		DaysOnList  int
		Libraries   []string
	}
	type bookKey struct{ Title, Author string }
	summaries := make(map[bookKey]*bookSummary)
	// preserve insertion order
	var order []bookKey

	for br := range resCh {
		k := bookKey{br.Book.Title, br.Book.Author}
		s, exists := summaries[k]
		if !exists {
			s = &bookSummary{Title: br.Book.Title, Author: br.Book.Author, DaysOnList: br.Book.DaysOnList}
			summaries[k] = s
			order = append(order, k)
		}
		s.Libraries = append(s.Libraries, br.Library)
		if br.Available {
			s.Available = true
		}
	}

	if len(summaries) == 0 {
		fmt.Fprintln(os.Stderr, "No to-read audiobooks are currently available or reservable at the specified libraries.")
		return
	}

	if outJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		out := make([]bookSummary, 0, len(order))
		for _, k := range order {
			out = append(out, *summaries[k])
		}
		_ = enc.Encode(out)
		return
	}

	// Sort: AVAILABLE first, then WAITLIST; alphabetical by title within each group.
	avail := make([]*bookSummary, 0, len(order))
	wait := make([]*bookSummary, 0, len(order))
	for _, k := range order {
		s := summaries[k]
		if s.Available {
			avail = append(avail, s)
		} else {
			wait = append(wait, s)
		}
	}

	for _, s := range append(avail, wait...) {
		status := "WAITLIST "
		if s.Available {
			status = "AVAILABLE"
		}
		libs := strings.Join(s.Libraries, ",")
		days := ""
		if s.DaysOnList > 0 {
			days = fmt.Sprintf("%dd", s.DaysOnList)
		}
		fmt.Printf("%-9s  %-40s  %-30s  %5s  %s\n",
			status,
			truncate(s.Title, 40),
			truncate(s.Author, 30),
			days,
			libs,
		)
	}
}

// truncate shortens s to at most n runes, appending "…" if trimmed.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

func main() {
	// Flags
	title := flag.String("title", "Lore", "Book title to search for")
	author := flag.String("author", "Alexandra Bracken", "Author (optional)")
	libs := flag.String("libs", "pittsburgh,chester,freelibrary", "Comma-separated library keys (thunder API keys)")
	outJSON := flag.Bool("json", false, "Output results as JSON")
	timeoutSec := flag.Int("timeout", 5, "Per-request timeout in seconds")
	parallel := flag.Int("parallel", 4, "Number of concurrent requests")
	ratePerSec := flag.Int("rate", 5, "Maximum HTTP requests per second (rate limit toward the Thunder API)")
	goodreadsFile := flag.String("goodreads", "", "Path to a Goodreads library export CSV; checks all to-read books")
	verbose := flag.Bool("verbose", false, "Print progress information to stderr")
	flag.Parse()

	// Build library list
	libTokens := strings.Split(*libs, ",")
	libraries := make([]Library, 0, len(libTokens))
	for _, t := range libTokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		// For user-friendly display we can try to fetch library details; for now use key as name
		libraries = append(libraries, Library{Name: t, Key: t})
	}

	ctx := context.Background()
	client := &http.Client{Timeout: time.Duration(*timeoutSec) * time.Second}

	// Shared rate limiter — all goroutines draw from the same token bucket.
	limiter, stopLimiter := newRateLimiter(*ratePerSec)
	defer close(stopLimiter)

	// --goodreads mode: scan every to-read book across all libraries.
	if *goodreadsFile != "" {
		books, err := parseGoodreadsToRead(*goodreadsFile)
		if err != nil {
			log.Fatalf("Error reading Goodreads export: %v", err)
		}
		if *verbose {
			fmt.Fprintf(os.Stderr, "Found %d to-read books in %s\n", len(books), *goodreadsFile)
		}
		runGoodreadsMode(ctx, client, limiter, books, libraries, *parallel, *outJSON, *verbose)
		return
	}

	book := BookQuery{Title: *title, Author: *author}

	sem := make(chan struct{}, *parallel)
	var wg sync.WaitGroup
	resCh := make(chan result, len(libraries))

	for _, lib := range libraries {
		wg.Add(1)
		sem <- struct{}{}
		go func(l Library) {
			defer wg.Done()
			defer func() { <-sem }()
			r := result{Library: l.Name}
			av, err := checkLibby(ctx, client, limiter, l, book)
			if err != nil {
				r.Error = err.Error()
				r.Found = false
				resCh <- r
				return
			}
			r.Found = true
			r.Available = av.IsAvailable
			r.Owned = av.OwnedCopies
			r.AvailableCopies = av.AvailableCopies
			r.Holds = av.HoldsCount
			resCh <- r
		}(lib)
	}

	wg.Wait()
	close(resCh)

	results := make([]result, 0, len(libraries))
	for rr := range resCh {
		results = append(results, rr)
	}

	if *outJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
		return
	}

	// Plain text output
	fmt.Printf("Checking availability of %q by %s\n\n", book.Title, book.Author)
	for _, r := range results {
		if r.Error != "" {
			fmt.Printf("  %-45s  ERROR: %s\n", r.Library, r.Error)
			continue
		}
		status := "Waitlist"
		if r.Available {
			status = "AVAILABLE NOW"
		}
		fmt.Printf("  %-45s  %s  (owned: %d, available: %d, holds: %d)\n", r.Library, status, r.Owned, r.AvailableCopies, r.Holds)
	}
}
