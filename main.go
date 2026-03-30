package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
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
	Title  string
	Author string
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
	Library   string `json:"library"`
	Found     bool   `json:"found"`
	Available bool   `json:"available"`
	Owned     int    `json:"ownedCopies"`
	AvailableCopies int `json:"availableCopies"`
	Holds     int    `json:"holdsCount"`
	Error     string `json:"error,omitempty"`
}

// doGetWithCtx performs a GET with context and simple retries/backoff.
func doGetWithCtx(ctx context.Context, client *http.Client, u string) ([]byte, error) {
	var lastErr error
	// simple retry loop
	for attempt := 0; attempt < 3; attempt++ {
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
		defer resp.Body.Close()
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
func getJSONWithCtx(ctx context.Context, client *http.Client, u string, out any) error {
	b, err := doGetWithCtx(ctx, client, u)
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
func checkLibby(ctx context.Context, client *http.Client, lib Library, book BookQuery) (availResponse, error) {
	var empty availResponse
	// Search for several results so we can prefer audiobook-format entries.
	searchURL := fmt.Sprintf("https://thunder.api.overdrive.com/v2/libraries/%s/media?query=%s&perPage=20", lib.Key, book.searchQuery())
	var sr searchResponse
	if err := getJSONWithCtx(ctx, client, searchURL, &sr); err != nil {
		return empty, fmt.Errorf("search request: %w", err)
	}
	if len(sr.Items) == 0 {
		return empty, fmt.Errorf("title %q not found in %s catalog", book.Title, lib.Name)
	}

	// Prefer an item that is an audiobook. We'll fetch each item's media detail and
	// choose the first where mediaType == "audiobook" or one of the formats contains "audiobook".
	var titleID string
	for _, item := range sr.Items {
		var md mediaDetail
		detailURL := fmt.Sprintf("https://thunder.api.overdrive.com/v2/libraries/%s/media/%s", lib.Key, item.ID)
		if err := getJSONWithCtx(ctx, client, detailURL, &md); err != nil {
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
	body2, err := doGetWithCtx(ctx, client, availURL)
	if err != nil {
		return empty, fmt.Errorf("availability request: %w", err)
	}
	var av availResponse
	if err := json.Unmarshal(body2, &av); err != nil {
		return empty, fmt.Errorf("availability parse: %w", err)
	}
	return av, nil
}

func main() {
	// Flags
	title := flag.String("title", "Lore", "Book title to search for")
	author := flag.String("author", "Alexandra Bracken", "Author (optional)")
	libs := flag.String("libs", "pittsburgh,chester,freelibrary", "Comma-separated library keys (thunder API keys)")
	outJSON := flag.Bool("json", false, "Output results as JSON")
	timeoutSec := flag.Int("timeout", 5, "Per-request timeout in seconds")
	parallel := flag.Int("parallel", 4, "Number of concurrent requests")
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

	book := BookQuery{Title: *title, Author: *author}

	ctx := context.Background()
	client := &http.Client{Timeout: time.Duration(*timeoutSec) * time.Second}

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
			av, err := checkLibby(ctx, client, l, book)
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
