package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Simple probe that searches a list of library keys for a title and prints
// the returned media items and their availability.

type SearchResp struct {
	Items []struct {
		ID              string `json:"id"`
		Title           string `json:"title"`
		FirstCreator    string `json:"firstCreatorName"`
		Formats         []struct{
			ID string `json:"id"`
		} `json:"formats"`
	} `json:"items"`
	TotalItems int `json:"totalItems"`
}

type AvailResp struct {
	IsAvailable     bool `json:"isAvailable"`
	AvailableCopies int  `json:"availableCopies"`
	OwnedCopies     int  `json:"ownedCopies"`
	HoldsCount      int  `json:"holdsCount"`
}

func getJSON(u string, out any) error {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return json.Unmarshal(b, out)
}

func probeLibrary(libKey, query string) error {
	fmt.Printf("\n=== Searching %s for %q ===\n", libKey, query)
	u := fmt.Sprintf("https://thunder.api.overdrive.com/v2/libraries/%s/media?query=%s&perPage=10", libKey, url.QueryEscape(query))
	var sr SearchResp
	if err := getJSON(u, &sr); err != nil {
		return fmt.Errorf("search failed: %w", err)
	}
	fmt.Printf("totalItems=%d\n", sr.TotalItems)
	for i, it := range sr.Items {
		fmt.Printf("%02d id=%s title=%q author=%q formats=[", i+1, it.ID, it.Title, it.FirstCreator)
		for j, f := range it.Formats {
			if j > 0 { fmt.Print(",") }
			fmt.Print(f.ID)
		}
		fmt.Println("]")

		// check availability for this media id
		au := fmt.Sprintf("https://thunder.api.overdrive.com/v2/libraries/%s/media/%s/availability", libKey, it.ID)
		var av AvailResp
		if err := getJSON(au, &av); err != nil {
			fmt.Printf("    availability: error: %v\n", err)
			continue
		}
		fmt.Printf("    availability: available=%v owned=%d availableCopies=%d holds=%d\n", av.IsAvailable, av.OwnedCopies, av.AvailableCopies, av.HoldsCount)
	}
	return nil
}

func main() {
	title := flag.String("title", "Lore", "Title to search for")
	libs := flag.String("libs", "pittsburgh,chester,freelibrary", "Comma-separated library keys")
	flag.Parse()
	// parse libs
	libKeys := []string{}
	for _, t := range splitNonEmpty(*libs, ',') {
		libKeys = append(libKeys, t)
	}
	for _, k := range libKeys {
		if err := probeLibrary(k, *title); err != nil {
			fmt.Fprintf(os.Stderr, "probe %s error: %v\n", k, err)
		}
	}
}

func splitNonEmpty(s string, sep rune) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == sep {
			if cur != "" { out = append(out, cur); cur = "" }
			continue
		}
		cur += string(r)
	}
	if cur != "" { out = append(out, cur) }
	return out
}
