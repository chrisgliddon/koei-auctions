package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Generic page fetching through the DataImpulse proxy (proxiedFetcher),
// for sites beyond the auction sources. Added 2026-11-16 at Chris's
// direction ("use the CLI and wildcard allow Itch") to pull itch.io game
// pages for the Epic Game Jam 2026 Airtable base.
//
// A site is fetchable only when registered here with a wildcard host
// pattern: "*.itch.io" allows itch.io itself and every subdomain (each
// itch game lives on its own <user>.itch.io host). URLs whose host does
// not match the pattern are refused — the registry is the allowlist.
//
// Runs are idempotent and resumable: a page already saved in the out dir
// is skipped, and fetch_state.json in the out dir is rewritten atomically
// after every page, so a killed run resumes where it stopped.

type fetchSite struct {
	Key     string
	Name    string
	Pattern string // wildcard host pattern, e.g. "*.itch.io"
}

var fetchSites = []fetchSite{
	{"itch", "Itch", "*.itch.io"},
}

func fetchSiteByKey(key string) (fetchSite, bool) {
	for _, s := range fetchSites {
		if s.Key == key {
			return s, true
		}
	}
	return fetchSite{}, false
}

// hostAllowed reports whether host matches a wildcard pattern of the form
// "*.example.com": the apex domain itself and any subdomain match.
func hostAllowed(pattern, host string) bool {
	if !strings.HasPrefix(pattern, "*.") {
		return host == pattern
	}
	apex := pattern[2:]
	return host == apex || strings.HasSuffix(host, "."+apex)
}

type fetchEntry struct {
	URL    string `json:"url"`
	Status string `json:"status"` // saved | skipped | failed
	Bytes  int    `json:"bytes,omitempty"`
	Error  string `json:"error,omitempty"`
	At     string `json:"at"`
}

type fetchState struct {
	Site    string              `json:"site"`
	Entries map[string]fetchEntry `json:"entries"`
}

func loadFetchState(path string) *fetchState {
	st := &fetchState{Entries: map[string]fetchEntry{}}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, st)
		if st.Entries == nil {
			st.Entries = map[string]fetchEntry{}
		}
	}
	return st
}

func saveFetchState(path string, st *fetchState) {
	raw, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err == nil {
		_ = os.Rename(tmp, path)
	}
}

// readFetchTargets parses the --urls file: one target per line, either
// "<id> <url>", "<id>,<url>", or a bare URL (the id then defaults to a
// sanitized host+path slug). Blank lines and #-comments are skipped.
func readFetchTargets(path string) ([][2]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ','
		})
		switch {
		case len(fields) >= 2:
			out = append(out, [2]string{fields[0], fields[1]})
		case len(fields) == 1 && strings.HasPrefix(fields[0], "http"):
			u, err := url.Parse(fields[0])
			if err != nil {
				return nil, fmt.Errorf("bad URL %q: %w", fields[0], err)
			}
			slug := strings.NewReplacer("/", "_", ":", "_").Replace(
				u.Host + u.Path)
			out = append(out, [2]string{slug, fields[0]})
		default:
			return nil, fmt.Errorf("unparsable line in %s: %q", path, line)
		}
	}
	return out, nil
}

func cmdFetch(cfg *config, args []string) error {
	var siteKey, urlsPath, outDir string
	limit := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--site":
			i++
			if i >= len(args) {
				return fmt.Errorf("--site needs a value")
			}
			siteKey = args[i]
		case "--urls":
			i++
			if i >= len(args) {
				return fmt.Errorf("--urls needs a value")
			}
			urlsPath = args[i]
		case "--out":
			i++
			if i >= len(args) {
				return fmt.Errorf("--out needs a value")
			}
			outDir = args[i]
		case "--limit":
			i++
			if i >= len(args) {
				return fmt.Errorf("--limit needs a value")
			}
			if _, err := fmt.Sscan(args[i], &limit); err != nil {
				return fmt.Errorf("bad --limit: %w", err)
			}
		default:
			return fmt.Errorf("unknown fetch flag %q", args[i])
		}
	}
	if siteKey == "" || urlsPath == "" || outDir == "" {
		return fmt.Errorf("fetch needs --site KEY --urls FILE --out DIR")
	}
	fs, ok := fetchSiteByKey(siteKey)
	if !ok {
		return fmt.Errorf("site %q is not registered for fetching (see fetchSites in fetch.go)", siteKey)
	}
	targets, err := readFetchTargets(urlsPath)
	if err != nil {
		return err
	}
	if limit > 0 && limit < len(targets) {
		targets = targets[:limit]
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	statePath := filepath.Join(outDir, "fetch_state.json")
	st := loadFetchState(statePath)
	st.Site = siteKey

	fetcher, err := newProxiedFetcher()
	if err != nil {
		return err
	}
	fmt.Printf("fetch: site %s (%s, pattern %s) via proxy %s; %d targets -> %s\n",
		fs.Key, fs.Name, fs.Pattern, maskProxyURL(fetcher.proxy), len(targets), outDir)

	saved, skipped, refused, failed := 0, 0, 0, 0
	for _, tgt := range targets {
		id, rawURL := tgt[0], tgt[1]
		u, err := url.Parse(rawURL)
		if err != nil || !hostAllowed(fs.Pattern, u.Host) {
			fmt.Printf("REFUSED %s %s (host outside %s)\n", id, rawURL, fs.Pattern)
			refused++
			continue
		}
		dest := filepath.Join(outDir, id+".html")
		if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
			skipped++
			continue
		}
		body, status, err := fetcher.fetch(rawURL)
		now := time.Now().UTC().Format(time.RFC3339)
		if err != nil {
			fmt.Printf("FAILED %s %s: %v\n", id, rawURL, err)
			st.Entries[id] = fetchEntry{URL: rawURL, Status: "failed", Error: err.Error(), At: now}
			failed++
			saveFetchState(statePath, st)
			continue
		}
		tmp := dest + ".tmp"
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, dest); err != nil {
			return err
		}
		st.Entries[id] = fetchEntry{URL: rawURL, Status: "saved", Bytes: len(body), At: now}
		saveFetchState(statePath, st)
		saved++
		if saved%50 == 0 {
			fmt.Printf("progress: saved %d, skipped %d, failed %d (last HTTP %d)\n",
				saved, skipped, failed, status)
		}
	}
	fmt.Printf("fetch done: saved %d, skipped %d, refused %d, failed %d; proxy attempts %d, fetched %d, 403s %d (geo-verified %d)\n",
		saved, skipped, refused, failed, fetcher.attempts, fetcher.fetched,
		fetcher.http403, fetcher.geoBlocked)
	return nil
}
