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
	"regexp"
	"slices"
	"sort"
	"strconv"
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
	Title      string
	Author     string
	DaysOnList int    // 0 when unknown (e.g. single-book mode)
	SeriesNote string // optional annotation shown in output (e.g. "not in your Goodreads")
	SeriesName string // series name, shown in series mode output
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
	DurationMinutes int  `json:"durationMinutes"` // populated from media detail, not the availability endpoint
}

// result is a structured result we can print or JSON encode.
type result struct {
	Library         string `json:"library"`
	Found           bool   `json:"found"`
	Available       bool   `json:"available"`
	Owned           int    `json:"ownedCopies"`
	AvailableCopies int    `json:"availableCopies"`
	Holds           int    `json:"holdsCount"`
	DurationMinutes int    `json:"durationMinutes,omitempty"`
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
	for attempt := range 3 {
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

		// Close the body explicitly — do NOT use defer here because we are
		// inside a retry loop and defer would hold every response body open
		// until the function returns (potential connection/memory leak).
		closeBody := func() {
			if cerr := resp.Body.Close(); cerr != nil {
				log.Printf("warning: failed to close response body: %v", cerr)
			}
		}

		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			closeBody()
			lastErr = fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			if resp.StatusCode == http.StatusTooManyRequests {
				backoff := time.Duration(attempt+1) * 5 * time.Second
				log.Printf("warning: rate-limited (HTTP 429) by Thunder API — backing off %s (attempt %d/3), consider lowering --rate", backoff, attempt+1)
				time.Sleep(backoff)
			} else {
				time.Sleep(time.Duration(attempt+1) * 300 * time.Millisecond)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				continue
			}
		}
		closeBody()
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

// lookupPageCount queries Google Books (then Open Library as fallback) for the
// page count of a book by title and author. Returns 0 if not found or on error.
func lookupPageCount(ctx context.Context, client *http.Client, limiter <-chan struct{}, title, author string) int {
	// --- Google Books ---
	q := "intitle:" + url.QueryEscape(title)
	if author != "" {
		q += "+inauthor:" + url.QueryEscape(author)
	}
	gbURL := fmt.Sprintf("https://www.googleapis.com/books/v1/volumes?q=%s&maxResults=3&fields=items/volumeInfo/pageCount", q)
	var gbResp struct {
		Items []struct {
			VolumeInfo struct {
				PageCount int `json:"pageCount"`
			} `json:"volumeInfo"`
		} `json:"items"`
	}
	if err := getJSONWithCtx(ctx, client, limiter, gbURL, &gbResp); err == nil {
		for _, item := range gbResp.Items {
			if item.VolumeInfo.PageCount > 0 {
				return item.VolumeInfo.PageCount
			}
		}
	}

	// --- Open Library fallback ---
	olURL := fmt.Sprintf("https://openlibrary.org/search.json?title=%s&fields=number_of_pages_median&limit=1",
		url.QueryEscape(title))
	if author != "" {
		olURL += "&author=" + url.QueryEscape(author)
	}
	var olResp struct {
		Docs []struct {
			Pages int `json:"number_of_pages_median"`
		} `json:"docs"`
	}
	if err := getJSONWithCtx(ctx, client, limiter, olURL, &olResp); err == nil {
		if len(olResp.Docs) > 0 && olResp.Docs[0].Pages > 0 {
			return olResp.Docs[0].Pages
		}
	}
	return 0
}

// mediaDetail contains minimal fields from the media detail endpoint we need to
// determine whether a result is an audiobook.
type mediaDetail struct {
	MediaType string `json:"mediaType"`
	Formats   []struct {
		ID       string `json:"id"`
		Duration string `json:"duration"` // "HH:MM:SS" — present on audiobook formats
	} `json:"formats"`
}

// parseDurationMinutes converts a "HH:MM:SS" string to total minutes (rounded down).
// Returns 0 if the string is empty or unparseable.
func parseDurationMinutes(s string) int {
	if s == "" {
		return 0
	}
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return 0
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0
	}
	return h*60 + m
}

// checkLibby returns the availability of the first audiobook result matching book at lib.
// The returned availResponse always has DurationMinutes populated when an audiobook edition
// was found, even if the function also returns a non-nil error (e.g. not owned by library).
func checkLibby(ctx context.Context, client *http.Client, limiter <-chan struct{}, lib Library, book BookQuery) (availResponse, error) {
	var empty availResponse
	// Search specifically for audiobook format so we don't waste detail lookups on ebooks.
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
	const maxDetailLookups = 20
	var titleID string
	var durationMinutes int
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
			for _, f := range md.Formats {
				if d := parseDurationMinutes(f.Duration); d > 0 {
					durationMinutes = d
					break
				}
			}
			break
		}
		for _, f := range md.Formats {
			if strings.Contains(strings.ToLower(f.ID), "audiobook") {
				titleID = item.ID
				if d := parseDurationMinutes(f.Duration); d > 0 {
					durationMinutes = d
				}
				break
			}
		}
		if titleID != "" {
			break
		}
	}
	// If we didn't find an audiobook edition among the returned items, fail.
	// Return whatever duration we found (may be zero) so callers can use it.
	if titleID == "" {
		return availResponse{DurationMinutes: durationMinutes},
			fmt.Errorf("no audiobook edition found in %s catalog for %q", lib.Name, book.Title)
	}

	availURL := fmt.Sprintf("https://thunder.api.overdrive.com/v2/libraries/%s/media/%s/availability", lib.Key, titleID)
	body2, err := doGetWithCtx(ctx, client, limiter, availURL)
	if err != nil {
		return availResponse{DurationMinutes: durationMinutes}, fmt.Errorf("availability request: %w", err)
	}
	var av availResponse
	if err := json.Unmarshal(body2, &av); err != nil {
		return availResponse{DurationMinutes: durationMinutes}, fmt.Errorf("availability parse: %w", err)
	}
	av.DurationMinutes = durationMinutes
	return av, nil
}

// goodreadsRow holds the fields we care about from the Goodreads CSV export.
type goodreadsRow struct {
	Title      string
	Author     string // "First Last" format (column "Author")
	DaysOnList int    // days since "Date Added" in the Goodreads export; 0 if unknown
	SeriesNote string // optional annotation, e.g. "not in Goodreads list"
	SeriesName string // series name, populated in series mode
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
	dateAddedIdx := colIdx["Date Added"] // optional
	_, hasDateAdded := colIdx["Date Added"]

	today := time.Now().Truncate(24 * time.Hour)

	// First pass: collect every title+author the user has already read.
	// We'll skip these even if they also appear on to-read (e.g. a book
	// exported twice with different shelf values).
	alreadyRead := make(map[string]struct{})
	type rawRow struct {
		title     string
		author    string
		shelf     string
		dateAdded string
	}
	var allRows []rawRow
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue // skip malformed rows
		}
		if len(rec) <= shelfIdx {
			continue
		}
		title := strings.TrimSpace(rec[titleIdx])
		author := strings.Join(strings.Fields(rec[authorIdx]), " ")
		shelf := strings.TrimSpace(rec[shelfIdx])
		var dateAdded string
		if hasDateAdded && dateAddedIdx < len(rec) {
			dateAdded = strings.TrimSpace(rec[dateAddedIdx])
		}
		if title == "" {
			continue
		}
		allRows = append(allRows, rawRow{title, author, shelf, dateAdded})
		shelfLower := strings.ToLower(shelf)
		if shelfLower == "read" {
			key := strings.ToLower(title) + "\x00" + strings.ToLower(author)
			alreadyRead[key] = struct{}{}
		}
	}

	// Second pass: collect to-read books, skipping anything already read
	// and deduplicating within the to-read set itself.
	seen := make(map[string]struct{})
	var books []goodreadsRow
	for _, row := range allRows {
		if strings.ToLower(row.shelf) != "to-read" {
			continue
		}
		key := strings.ToLower(row.title) + "\x00" + strings.ToLower(row.author)
		if _, done := alreadyRead[key]; done {
			log.Printf("skipping %q — already marked as read", row.title)
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		var days int
		if row.dateAdded != "" {
			if t, err := time.Parse("2006/01/02", row.dateAdded); err == nil {
				days = int(today.Sub(t.Truncate(24*time.Hour)).Hours() / 24)
			}
		}
		books = append(books, goodreadsRow{Title: row.title, Author: row.author, DaysOnList: days})
	}
	return books, nil
}

// seriesRE matches the Goodreads series suffix in a title, e.g.:
//
//	"The Name of the Wind (Kingkiller Chronicle, #1)"  →  series="Kingkiller Chronicle", num="1"
//	"A Memory of Light (The Wheel of Time, #14)"       →  series="The Wheel of Time",    num="14"
var seriesRE = regexp.MustCompile(`\(([^)]+),\s*#([0-9]+(?:\.[0-9]+)?)\)\s*$`)

// seriesSlugRE extracts the Goodreads series slug from a book page, e.g. "/series/45015-will-trent"
var seriesSlugRE = regexp.MustCompile(`/series/([0-9]+-[a-z0-9-]+)`)

// seriesBookRE extracts (series tag, position, bare title) from the entity-encoded JSON
// embedded in a Goodreads series page, e.g.:
//
//	&quot;title&quot;:&quot;Triptych (Will Trent, #1)&quot; ... &quot;bookTitleBare&quot;:&quot;Triptych&quot;
var seriesBookRE = regexp.MustCompile(
	`&quot;title&quot;:&quot;[^&]+\(([^,&]+),\s*#([0-9]+(?:\.[0-9]+)?)\)&quot;` +
		`.*?&quot;bookTitleBare&quot;:&quot;([^&]+)&quot;`)

// seriesEntry is one book in a series as recorded in the Goodreads export.
type seriesEntry struct {
	GoodreadsID string  // from the "Book Id" CSV column
	Num         float64 // series position (1, 2, 3 …)
	Title       string  // clean title without the series suffix
	Author      string
	Shelf       string // "read", "to-read", "currently-reading", …
}

// goodreadsSeriesPage fetches the Goodreads book page for goodreadsID,
// extracts the series page slug, fetches that series page, and returns
// all (position, bare title) pairs found there.
// Returns nil, nil if the series page cannot be determined or parsed.
func goodreadsSeriesPage(goodreadsID string) ([][2]string, error) {
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	// Step 1: fetch the book page to find the series slug.
	bookURL := "https://www.goodreads.com/book/show/" + goodreadsID
	req, _ := http.NewRequest("GET", bookURL, nil)
	req.Header.Set("User-Agent", ua)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch book page: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); cerr != nil {
		log.Printf("warning: failed to close book page response body: %v", cerr)
	}
	if err != nil {
		return nil, fmt.Errorf("read book page: %w", err)
	}

	m := seriesSlugRE.FindSubmatch(body)
	if m == nil {
		return nil, nil // book not part of a series on Goodreads
	}
	slug := string(m[1])

	// Step 2: fetch the series page.
	seriesURL := "https://www.goodreads.com/series/" + slug
	req2, _ := http.NewRequest("GET", seriesURL, nil)
	req2.Header.Set("User-Agent", ua)
	resp2, err := client.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("fetch series page: %w", err)
	}
	body2, err := io.ReadAll(resp2.Body)
	if cerr := resp2.Body.Close(); cerr != nil {
		log.Printf("warning: failed to close series page response body: %v", cerr)
	}
	if err != nil {
		return nil, fmt.Errorf("read series page: %w", err)
	}

	// Step 3: extract all (position, bare title) pairs from embedded JSON.
	matches := seriesBookRE.FindAllSubmatch(body2, -1)
	seen := make(map[float64]bool)
	var results [][2]string // [position_str, bare_title]
	for _, m2 := range matches {
		posStr := string(m2[2])
		pos, _ := strconv.ParseFloat(posStr, 64)
		if seen[pos] {
			continue
		}
		seen[pos] = true
		// HTML-unescape the bare title (handles &amp; etc.)
		bare := strings.NewReplacer(
			"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'",
		).Replace(string(m2[3]))
		results = append(results, [2]string{posStr, bare})
	}
	return results, nil
}

// parseSeriesNextBooks reads the full Goodreads export and returns, for every
// series the user has started reading, the next book they haven't read yet.
// It first checks the CSV for the next book; if not found there, it looks up
// the Goodreads series page to find it. Each returned row has a SeriesNote
// indicating whether the book was found in the CSV or discovered via Goodreads.
func parseSeriesNextBooks(ctx context.Context, path string, verbose bool, skipNovellas bool) ([]goodreadsRow, error) {
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
	bookIDIdx, hasBookID := colIdx["Book Id"]

	// Collect every series entry from the CSV (all shelves).
	type seriesData struct {
		entries  []seriesEntry
		dispName string // canonical series name for display
	}
	seriesMap := make(map[string]*seriesData) // key = lowercase series name

	// Also track all read titles globally (normalized) so we can skip books
	// the user has already read under a different series name (e.g. a novella
	// that appears in both "World of the Five Gods" and "Penric and Desdemona").
	readTitles := make(map[string]struct{})

	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(rec) <= shelfIdx {
			continue
		}
		rawTitle := strings.TrimSpace(rec[titleIdx])
		shelf := strings.TrimSpace(rec[shelfIdx])
		shelfLower := strings.ToLower(shelf)

		// Track every read title globally, even books not in a series.
		if shelfLower == "read" {
			readTitles[strings.ToLower(strings.TrimSpace(rawTitle))] = struct{}{}
		}

		m := seriesRE.FindStringSubmatch(rawTitle)
		if m == nil {
			continue
		}
		series := strings.TrimSpace(m[1])
		num, _ := strconv.ParseFloat(m[2], 64)
		cleanTitle := strings.TrimSpace(rawTitle[:strings.LastIndex(rawTitle, "(")])
		author := strings.Join(strings.Fields(rec[authorIdx]), " ")
		var bookID string
		if hasBookID && bookIDIdx < len(rec) {
			bookID = strings.TrimSpace(rec[bookIDIdx])
		}

		// Also index the clean title for cross-series read checking.
		if shelfLower == "read" {
			readTitles[strings.ToLower(cleanTitle)] = struct{}{}
		}

		key := strings.ToLower(series)
		if _, exists := seriesMap[key]; !exists {
			seriesMap[key] = &seriesData{dispName: series}
		}
		seriesMap[key].entries = append(seriesMap[key].entries, seriesEntry{
			GoodreadsID: bookID,
			Num:         num,
			Title:       cleanTitle,
			Author:      author,
			Shelf:       shelfLower,
		})
	}

	// seriesWork holds everything needed to look up one series.
	type seriesWork struct {
		sd                  *seriesData
		maxRead             float64
		readBookID          string
		readAuthor          string
		currentlyReadingNum float64
	}

	// First pass: resolve all CSV-resident candidates immediately (no network
	// needed) and collect the ones that require a Goodreads page fetch.
	var next []goodreadsRow
	var needFetch []seriesWork
	var mu sync.Mutex // guards next and needFetch appends

	for _, sd := range seriesMap {
		entries := sd.entries

		var maxRead float64
		var readBookID, readAuthor string
		var currentlyReadingNum float64
		hasStarted := false
		for _, e := range entries {
			if e.Shelf == "read" || e.Shelf == "currently-reading" {
				hasStarted = true
				if e.Num > maxRead {
					maxRead = e.Num
					readBookID = e.GoodreadsID
					readAuthor = e.Author
				}
				if e.Shelf == "currently-reading" && e.Num > currentlyReadingNum {
					currentlyReadingNum = e.Num
				}
			}
		}
		if !hasStarted {
			continue
		}

		var csvCandidate *seriesEntry
		for i := range entries {
			e := &entries[i]
			if e.Shelf == "read" || e.Num <= maxRead {
				continue
			}
			if csvCandidate == nil || e.Num < csvCandidate.Num {
				csvCandidate = e
			}
		}

		if csvCandidate != nil {
			if _, alreadyRead := readTitles[strings.ToLower(csvCandidate.Title)]; alreadyRead {
				if verbose {
					_, _ = fmt.Fprintf(os.Stderr, "  [series lookup] %q — skipping %q (already read under another series)\n", sd.dispName, csvCandidate.Title)
				}
				continue
			}
			seriesNote := "in your Goodreads"
			if currentlyReadingNum > 0 && csvCandidate.Num == currentlyReadingNum+1 {
				seriesNote = fmt.Sprintf("up next — currently reading #%.4g", currentlyReadingNum)
			}
			log.Printf("series: %q — read up to #%.4g, next is #%.4g %q (in Goodreads)",
				sd.dispName, maxRead, csvCandidate.Num, csvCandidate.Title)
			next = append(next, goodreadsRow{
				Title:      csvCandidate.Title,
				Author:     csvCandidate.Author,
				SeriesNote: seriesNote,
				SeriesName: sd.dispName,
			})
			continue
		}

		if readBookID == "" {
			continue
		}
		needFetch = append(needFetch, seriesWork{
			sd:                  sd,
			maxRead:             maxRead,
			readBookID:          readBookID,
			readAuthor:          readAuthor,
			currentlyReadingNum: currentlyReadingNum,
		})
	}

	// Second pass: fan out Goodreads series page fetches across a worker pool.
	const seriesWorkers = 4
	workCh := make(chan seriesWork, len(needFetch))
	for _, w := range needFetch {
		workCh <- w
	}
	close(workCh)

	var wg sync.WaitGroup
	for range seriesWorkers {
		wg.Go(func() {
			for w := range workCh {
				select {
				case <-ctx.Done():
					return
				default:
				}

				if verbose {
					_, _ = fmt.Fprintf(os.Stderr, "  [series lookup] %q — fetching Goodreads series page...\n", w.sd.dispName)
				}

				var seriesBooks [][2]string
				var err error
				for attempt := 1; attempt <= 3; attempt++ {
					seriesBooks, err = goodreadsSeriesPage(w.readBookID)
					if err == nil {
						break
					}
					if attempt < 3 {
						backoff := time.Duration(attempt*attempt) * 5 * time.Second
						if verbose {
							_, _ = fmt.Fprintf(os.Stderr, "  [series lookup] %q — attempt %d failed, retrying in %v...\n", w.sd.dispName, attempt, backoff)
						}
						time.Sleep(backoff)
					}
				}
				if err != nil {
					log.Printf("warning: series lookup for %q failed after 3 attempts: %v", w.sd.dispName, err)
					continue
				}

				for _, pair := range seriesBooks {
					pos, _ := strconv.ParseFloat(pair[0], 64)
					if pos <= w.maxRead {
						continue
					}
					if skipNovellas && pos != float64(int(pos)) {
						continue
					}
					if _, alreadyRead := readTitles[strings.ToLower(pair[1])]; alreadyRead {
						if verbose {
							_, _ = fmt.Fprintf(os.Stderr, "  [series lookup] %q — skipping %q (already read under another series)\n", w.sd.dispName, pair[1])
						}
						continue
					}
					seriesNote := "not in your Goodreads"
					if w.currentlyReadingNum > 0 && pos == w.currentlyReadingNum+1 {
						seriesNote = fmt.Sprintf("up next — currently reading #%.4g", w.currentlyReadingNum)
					}
					log.Printf("series: %q — read up to #%.4g, next is #%.4g %q (not in Goodreads)",
						w.sd.dispName, w.maxRead, pos, pair[1])
					mu.Lock()
					next = append(next, goodreadsRow{
						Title:      pair[1],
						Author:     w.readAuthor,
						SeriesNote: seriesNote,
						SeriesName: w.sd.dispName,
					})
					mu.Unlock()
					break
				}
			}
		})
	}
	wg.Wait()

	return next, nil
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
	outCSV bool,
	seriesMode bool,
	audibleMode bool,
	verbose bool,
) {
	// Deduplicate books by title — the same book can appear under two series
	// names (e.g. "George Smiley" and "George Smiley, #6; Karla Trilogy").
	seenTitles := make(map[string]struct{}, len(books))
	deduped := make([]goodreadsRow, 0, len(books))
	for _, b := range books {
		k := strings.ToLower(b.Title)
		if _, seen := seenTitles[k]; seen {
			continue
		}
		seenTitles[k] = struct{}{}
		deduped = append(deduped, b)
	}
	books = deduped

	total := len(books)
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	var completed atomic.Int64
	resCh := make(chan bookResult, total*len(libraries))
	// missCh carries duration info for books that had no hit at a library,
	// used to build the audible list.
	type missResult struct {
		Book            BookQuery
		DurationMinutes int
	}
	missCh := make(chan missResult, total*len(libraries))

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
					// Not found or no audiobook edition — send to miss channel
					// so audible mode can account for it.
					if audibleMode {
						missCh <- missResult{Book: b, DurationMinutes: av.DurationMinutes}
					}
					return
				}
				// Only report if available NOW or reservable (library owns
				// copies but they're all checked out → holds queue open).
				reservable := av.OwnedCopies > 0 && !av.IsAvailable
				if !av.IsAvailable && !reservable {
					if audibleMode {
						missCh <- missResult{Book: b, DurationMinutes: av.DurationMinutes}
					}
					return
				}
				br.Found = true
				br.Available = av.IsAvailable
				br.Owned = av.OwnedCopies
				br.AvailableCopies = av.AvailableCopies
				br.Holds = av.HoldsCount
				br.DurationMinutes = av.DurationMinutes
				resCh <- br
				if verbose {
					status := "WAITLIST"
					if br.Available {
						status = "AVAILABLE"
					}
					_, _ = fmt.Fprintf(os.Stderr, "  [hit] %s @ %s — %s\n", truncate(b.Title, 50), l.Name, status)
				}
			}(bq, lib)
		}

		// Progress tick: once per book (after dispatching all library goroutines
		// for that book) so the counter reflects books dispatched, not completed.
		// We use a separate goroutine-safe counter on completion instead.
		n := completed.Add(1)
		if verbose && n%10 == 0 {
			_, _ = fmt.Fprintf(os.Stderr, "  [progress] %d / %d books dispatched...\n", n, total)
		}
	}

	wg.Wait()
	close(resCh)
	close(missCh)
	// Collect hits and collapse per-library results into one row per book.
	// A book is AVAILABLE if any library has it now; otherwise WAITLIST.
	type bookSummary struct {
		Title           string
		Author          string
		Available       bool
		DaysOnList      int
		SeriesNote      string
		SeriesName      string
		Libraries       []string
		DurationMinutes int
	}
	type bookKey struct{ Title, Author string }
	summaries := make(map[bookKey]*bookSummary)
	// preserve insertion order
	var order []bookKey

	for br := range resCh {
		k := bookKey{br.Book.Title, br.Book.Author}
		s, exists := summaries[k]
		if !exists {
			s = &bookSummary{
				Title:           br.Book.Title,
				Author:          br.Book.Author,
				DaysOnList:      br.Book.DaysOnList,
				SeriesNote:      br.Book.SeriesNote,
				SeriesName:      br.Book.SeriesName,
				DurationMinutes: br.DurationMinutes,
			}
			summaries[k] = s
			order = append(order, k)
		}
		// Deduplicate library names.
		alreadyHasLib := slices.Contains(s.Libraries, br.Library)
		if !alreadyHasLib {
			s.Libraries = append(s.Libraries, br.Library)
		}
		if br.Available {
			s.Available = true
		}
		// Keep the first non-zero duration we see across libraries.
		if s.DurationMinutes == 0 && br.DurationMinutes > 0 {
			s.DurationMinutes = br.DurationMinutes
		}
	}

	if len(summaries) == 0 && !audibleMode {
		_, _ = fmt.Fprintln(os.Stderr, "No to-read audiobooks are currently available or reservable at the specified libraries.")
		return
	}

	if !audibleMode {
		// Normal output — AVAILABLE/WAITLIST only.
		if len(summaries) == 0 {
			_, _ = fmt.Fprintln(os.Stderr, "No to-read audiobooks are currently available or reservable at the specified libraries.")
			return
		}
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
		if outCSV {
			w := csv.NewWriter(os.Stdout)
			if seriesMode {
				_ = w.Write([]string{"Status", "Title", "Series", "In Goodreads", "Author", "Duration", "Minutes", "Libraries"})
			} else {
				_ = w.Write([]string{"Status", "Title", "Author", "Days", "Duration", "Minutes", "Libraries"})
			}
			for _, s := range append(avail, wait...) {
				status := "WAITLIST"
				if s.Available {
					status = "AVAILABLE"
				}
				durHuman, durMins := "", ""
				if s.DurationMinutes > 0 {
					durHuman = formatDuration(s.DurationMinutes)
					durMins = strconv.Itoa(s.DurationMinutes)
				}
				if seriesMode {
					toRead := ""
					if s.SeriesNote == "in your Goodreads" {
						toRead = "to-read"
					}
					_ = w.Write([]string{status, s.Title, s.SeriesName, toRead, s.Author, durHuman, durMins, strings.Join(s.Libraries, ",")})
				} else {
					days := ""
					if s.DaysOnList > 0 {
						days = strconv.Itoa(s.DaysOnList)
					}
					_ = w.Write([]string{status, s.Title, s.Author, days, durHuman, durMins, strings.Join(s.Libraries, ",")})
				}
			}
			w.Flush()
			return
		}
		for _, s := range append(avail, wait...) {
			status := "WAITLIST "
			if s.Available {
				status = "AVAILABLE"
			}
			dur := ""
			if s.DurationMinutes > 0 {
				dur = formatDuration(s.DurationMinutes)
			}
			if seriesMode {
				toRead := ""
				if s.SeriesNote == "in your Goodreads" {
					toRead = "to-read"
				}
				fmt.Printf("%-9s  %-55s  %-35s  %-9s  %-30s  %-10s  %s\n",
					status, truncate(s.Title, 55), truncate(s.SeriesName, 35),
					toRead, truncate(s.Author, 30), dur, strings.Join(s.Libraries, ","),
				)
			} else {
				days := ""
				if s.DaysOnList > 0 {
					days = fmt.Sprintf("%dd", s.DaysOnList)
				}
				fmt.Printf("%-9s  %-55s  %-30s  %-6s  %-10s  %s\n",
					status, truncate(s.Title, 55), truncate(s.Author, 30),
					days, dur, strings.Join(s.Libraries, ","),
				)
			}
		}
		return
	}

	// --- Audible mode: output only books with zero Libby hits ---
	type audibleEntry struct {
		Book  BookQuery
		Pages int
	}
	// Collect misses.
	bestMiss := make(map[bookKey]*audibleEntry)
	for mr := range missCh {
		k := bookKey{mr.Book.Title, mr.Book.Author}
		if _, ok := bestMiss[k]; !ok {
			bestMiss[k] = &audibleEntry{Book: mr.Book}
		}
	}
	// Only include books that had NO hit in any library.
	seen := make(map[bookKey]struct{})
	var audibleEntries []*audibleEntry
	for _, row := range books {
		k := bookKey{row.Title, row.Author}
		if _, already := seen[k]; already {
			continue
		}
		seen[k] = struct{}{}
		if _, hasHit := summaries[k]; hasHit {
			continue // book is available or reservable in Libby — skip
		}
		if e, isMiss := bestMiss[k]; isMiss {
			audibleEntries = append(audibleEntries, e)
		}
	}

	// For entries with no audio duration, look up page count from Open Library.
	if verbose {
		_, _ = fmt.Fprintf(os.Stderr, "Looking up page counts for %d audible candidates...\n", len(audibleEntries))
	}
	pageSem := make(chan struct{}, parallel)
	var pageWg sync.WaitGroup
	var pageCompleted atomic.Int64
	for _, e := range audibleEntries {
		pageWg.Add(1)
		pageSem <- struct{}{}
		go func(entry *audibleEntry) {
			defer pageWg.Done()
			defer func() { <-pageSem }()
			entry.Pages = lookupPageCount(ctx, client, limiter, entry.Book.Title, entry.Book.Author)
			n := pageCompleted.Add(1)
			if verbose {
				pages := "no page count found"
				if entry.Pages > 0 {
					pages = fmt.Sprintf("%d pages", entry.Pages)
				}
				_, _ = fmt.Fprintf(os.Stderr, "  [pages] %s — %s\n", truncate(entry.Book.Title, 50), pages)
				if n%10 == 0 {
					_, _ = fmt.Fprintf(os.Stderr, "  [progress] %d / %d page lookups done...\n", n, int64(len(audibleEntries)))
				}
			}
		}(e)
	}
	pageWg.Wait()
	if verbose {
		_, _ = fmt.Fprintf(os.Stderr, "Page lookups complete (%d books)\n", len(audibleEntries))
	}

	sort.Slice(audibleEntries, func(i, j int) bool {
		return strings.ToLower(audibleEntries[i].Book.Title) < strings.ToLower(audibleEntries[j].Book.Title)
	})

	if len(audibleEntries) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "No unavailable audiobooks found for Audible list.")
		return
	}

	if outCSV {
		w := csv.NewWriter(os.Stdout)
		_ = w.Write([]string{"Title", "Author", "Pages"})
		for _, e := range audibleEntries {
			pages := ""
			if e.Pages > 0 {
				pages = strconv.Itoa(e.Pages)
			}
			_ = w.Write([]string{e.Book.Title, e.Book.Author, pages})
		}
		w.Flush()
		return
	}

	for _, e := range audibleEntries {
		size := ""
		if e.Pages > 0 {
			size = fmt.Sprintf("%d pages", e.Pages)
		}
		fmt.Printf("%-55s  %-30s  %s\n",
			truncate(e.Book.Title, 55), truncate(e.Book.Author, 30), size,
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

// formatDuration converts a duration in minutes to a human-readable string
// like "12h 34m". Returns an empty string for zero.
func formatDuration(minutes int) string {
	if minutes <= 0 {
		return ""
	}
	h := minutes / 60
	m := minutes % 60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func main() {
	// Flags
	title := flag.String("title", "Lore", "Book title to search for")
	author := flag.String("author", "Alexandra Bracken", "Author (optional)")
	libs := flag.String("libs", "pittsburgh,chester,freelibrary", "Comma-separated library keys (thunder API keys)")
	outJSON := flag.Bool("json", false, "Output results as JSON")
	outCSV := flag.Bool("csv", false, "Output results as CSV (suitable for import into Google Sheets)")
	audible := flag.Bool("audible", false, "Output books unavailable in any library, sorted longest first (for best Audible credit value)")
	timeoutSec := flag.Int("timeout", 15, "Per-request timeout in seconds")
	parallel := flag.Int("parallel", 8, "Number of concurrent requests")
	ratePerSec := flag.Int("rate", 20, "Maximum HTTP requests per second (rate limit toward the Thunder API)")
	goodreadsFile := flag.String("goodreads", "", "Path to a Goodreads library export CSV; checks all to-read books")
	seriesMode := flag.Bool("series", false, "Check next-in-series books for series you have started (requires --goodreads)")
	skipNovellas := flag.Bool("skip-novellas", false, "In series mode, skip novellas and short stories (non-integer positions like #1.5)")
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
		if *seriesMode {
			// --series mode: find next-in-series books for series you have started.
			books, err := parseSeriesNextBooks(ctx, *goodreadsFile, *verbose, *skipNovellas)
			if err != nil {
				log.Fatalf("Error reading Goodreads export: %v", err)
			}
			if len(books) == 0 {
				_, _ = fmt.Fprintln(os.Stderr, "No incomplete series found.")
				return
			}
			if *verbose {
				_, _ = fmt.Fprintf(os.Stderr, "Found %d next-in-series books to check\n", len(books))
			}
			runGoodreadsMode(ctx, client, limiter, books, libraries, *parallel, *outJSON, *outCSV, true, *audible, *verbose)
			return
		}
		books, err := parseGoodreadsToRead(*goodreadsFile)
		if err != nil {
			log.Fatalf("Error reading Goodreads export: %v", err)
		}
		if *verbose {
			_, _ = fmt.Fprintf(os.Stderr, "Found %d to-read books in %s\n", len(books), *goodreadsFile)
		}
		runGoodreadsMode(ctx, client, limiter, books, libraries, *parallel, *outJSON, *outCSV, false, *audible, *verbose)
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
