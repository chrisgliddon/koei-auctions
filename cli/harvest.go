package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"net/url"
	"time"
)

// harvest fetches Yahoo Auctions search + item pages through the DataImpulse
// proxy (never direct, never ZenMarket), saves every item page to the local
// HTML corpus, parses the __NEXT_DATA__ payload, and produces sweep-shaped
// candidates. It deliberately does NOT set koei_evidence: Koei Tecmo
// confirmation stays an agent photo/publisher check before anything is
// written to Airtable (standing gate, 2026-11-02).

type watchEntry struct {
	En   string `json:"en"`
	Jp   string `json:"jp"`
	ID   string `json:"game_record_id"`
	Own  bool   `json:"-"`
}

type yahooItem struct {
	AuctionID string `json:"auctionId"`
	Title     string `json:"title"`
	Price     int    `json:"price"`
	Bids      int    `json:"bids"`
	EndTime   string `json:"endTime"`
	Status    string `json:"status"`
	Seller    struct {
		AucUserID string `json:"aucUserId"`
	} `json:"seller"`
	Img []struct {
		Image string `json:"image"`
	} `json:"img"`
	Category struct {
		Path []struct {
			Name string `json:"name"`
		} `json:"path"`
	} `json:"category"`
}

type nextData struct {
	Props struct {
		PageProps struct {
			InitialState struct {
				Item struct {
					Detail struct {
						Item yahooItem `json:"item"`
					} `json:"detail"`
				} `json:"item"`
			} `json:"initialState"`
		} `json:"pageProps"`
	} `json:"props"`
}

var (
	nextDataRe = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__" type="application/json">(.*?)</script>`)
	searchIDRe = regexp.MustCompile(`/jp/auction/([a-z0-9]+)`)
)

func parseYahooItem(html []byte) (*yahooItem, error) {
	m := nextDataRe.FindSubmatch(html)
	if m == nil {
		return nil, fmt.Errorf("no __NEXT_DATA__ in page")
	}
	var nd nextData
	if err := json.Unmarshal(m[1], &nd); err != nil {
		return nil, fmt.Errorf("__NEXT_DATA__ parse: %w", err)
	}
	it := nd.Props.PageProps.InitialState.Item.Detail.Item
	if it.AuctionID == "" {
		return nil, fmt.Errorf("no item in __NEXT_DATA__")
	}
	return &it, nil
}

func (it *yahooItem) categoryPath() string {
	names := make([]string, 0, len(it.Category.Path))
	for _, p := range it.Category.Path {
		names = append(names, p.Name)
	}
	return strings.Join(names, " > ")
}

// inScopeCategory is the category-only rule enforced from the item's own
// breadcrumb: game software (console/handheld) or computer categories only.
func inScopeCategory(path string) bool {
	return strings.Contains(path, "ゲーム") || strings.Contains(path, "コンピュータ") || strings.Contains(path, "パソコン")
}

type harvestState struct {
	Titles map[string]struct {
		LastRun string   `json:"last_run"`
		Seen    []string `json:"seen"`
	} `json:"titles"`
}

func loadHarvestState(path string) *harvestState {
	st := &harvestState{Titles: map[string]struct {
		LastRun string   `json:"last_run"`
		Seen    []string `json:"seen"`
	}{}}
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, st)
		if st.Titles == nil {
			st.Titles = map[string]struct {
				LastRun string   `json:"last_run"`
				Seen    []string `json:"seen"`
			}{}
		}
	}
	return st
}

func saveHarvestState(path string, st *harvestState) {
	raw, _ := json.MarshalIndent(st, "", " ")
	tmp := path + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		os.Rename(tmp, path)
	}
}

func knownYahooIDs(db *sql.DB) map[string]bool {
	known := map[string]bool{}
	rows, err := db.Query(`SELECT auction_id FROM auctions WHERE source IN ('Yahoo','PayPay Flea') AND auction_id IS NOT NULL`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				known[id] = true
			}
		}
	}
	rows2, err := db.Query(`SELECT external_id FROM staging WHERE source_key='yahoo'`)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var id string
			if rows2.Scan(&id) == nil {
				known[id] = true
			}
		}
	}
	return known
}

func cmdHarvest(cfg *config, args []string) error {
	var titleFilter []string
	perTitle := 10
	doIngest := false
	outPath := ""
	corpusDir := homeExpand("~/workspace/koei-auctions/html/yahoo")
	watchPath := homeExpand("~/workspace/koei-watchlist/watchlist.json")
	statePath := homeExpand("~/workspace/goals/koei-watchlist-monitoring/hidden_files/harvest_state.json")
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--titles":
			if i+1 < len(args) {
				titleFilter = strings.Split(args[i+1], ",")
				i++
			}
		case "--per-title":
			if i+1 < len(args) {
				fmt.Sscan(args[i+1], &perTitle)
				i++
			}
		case "--ingest":
			doIngest = true
		case "--out":
			if i+1 < len(args) {
				outPath = args[i+1]
				i++
			}
		case "--corpus":
			if i+1 < len(args) {
				corpusDir = args[i+1]
				i++
			}
		case "--watchlist":
			if i+1 < len(args) {
				watchPath = args[i+1]
				i++
			}
		default:
			return fmt.Errorf("unknown harvest flag %s", args[i])
		}
	}
	raw, err := os.ReadFile(watchPath)
	if err != nil {
		return err
	}
	var wl struct {
		Watch []watchEntry `json:"watch"`
		Owned []watchEntry `json:"owned_variant_watch"`
	}
	if err := json.Unmarshal(raw, &wl); err != nil {
		return fmt.Errorf("watchlist parse: %w", err)
	}
	entries := append(wl.Watch, wl.Owned...)
	if len(titleFilter) > 0 {
		var sel []watchEntry
		for _, e := range entries {
			for _, f := range titleFilter {
				if strings.Contains(e.Jp, strings.TrimSpace(f)) || strings.Contains(e.En, strings.TrimSpace(f)) {
					sel = append(sel, e)
				}
			}
		}
		entries = sel
	}
	if len(entries) == 0 {
		return fmt.Errorf("no watchlist titles selected")
	}
	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		return err
	}
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	known := knownYahooIDs(db)
	fetcher, err := newProxiedFetcher()
	if err != nil {
		return err
	}
	fmt.Printf("harvest: %d titles, proxy %s, per-title cap %d\n", len(entries), maskProxyURL(fetcher.proxy), perTitle)
	state := loadHarvestState(statePath)
	var all []candidate
	blockedStreak := 0
	for _, e := range entries {
		searchURL := "https://auctions.yahoo.co.jp/search/search?p=" + url.QueryEscape(e.Jp) + "&n=100"
		body, err := fetcher.fetch(searchURL)
		if err != nil {
			fmt.Printf("[%s] search failed: %v\n", e.Jp, err)
			blockedStreak++
			if blockedStreak >= 3 {
				fmt.Println("source blocked on 3 consecutive titles - stopping (honest null for the rest)")
				break
			}
			continue
		}
		blockedStreak = 0
		var ids []string
		seenHere := map[string]bool{}
		for _, m := range searchIDRe.FindAllSubmatch(body, -1) {
			id := string(m[1])
			if !seenHere[id] {
				seenHere[id] = true
				ids = append(ids, id)
			}
		}
		newCount, fetchedCount := 0, 0
		for _, id := range ids {
			if fetchedCount >= perTitle {
				break
			}
			if known[id] {
				continue
			}
			known[id] = true
			fetchedCount++
			page, err := fetcher.fetch("https://auctions.yahoo.co.jp/jp/auction/" + id)
			if err != nil {
				fmt.Printf("  %s: item fetch failed: %v\n", id, err)
				continue
			}
			os.WriteFile(filepath.Join(corpusDir, id+".html"), page, 0o644)
			it, err := parseYahooItem(page)
			if err != nil {
				fmt.Printf("  %s: parse failed: %v\n", id, err)
				continue
			}
			if it.AuctionID != id {
				fmt.Printf("  %s: IDENTITY MISMATCH - page is %s, not written\n", id, it.AuctionID)
				continue
			}
			cat := it.categoryPath()
			if !inScopeCategory(cat) {
				fmt.Printf("  %s: out of category (%s) - dropped\n", id, cat)
				continue
			}
			photos := []string{}
			for _, im := range it.Img {
				if len(photos) >= 8 {
					break
				}
				photos = append(photos, im.Image)
			}
			bids := it.Bids
			all = append(all, candidate{
				ID: it.AuctionID, Title: it.Title, Price: it.Price, Photos: photos,
				Seller: it.Seller.AucUserID, Bids: &bids, EndsAt: it.EndTime,
				GameID: e.ID, PageURL: "https://auctions.yahoo.co.jp/jp/auction/" + it.AuctionID,
			})
			newCount++
		}
		entry := state.Titles[e.Jp]
		entry.LastRun = time.Now().Format("2006-01-02")
		for id := range seenHere {
			entry.Seen = append(entry.Seen, id)
		}
		state.Titles[e.Jp] = entry
		saveHarvestState(statePath, state)
		fmt.Printf("[%s] %d search hits, %d new candidates\n", e.Jp, len(ids), newCount)
	}
	if outPath == "" {
		outPath = homeExpand("~/workspace/goals/koei-watchlist-monitoring/hidden_files/candidates_yahoo_cli_" + time.Now().Format("2006-01-02") + ".json")
	}
	rawOut, _ := json.MarshalIndent(all, "", " ")
	if err := os.WriteFile(outPath, rawOut, 0o644); err != nil {
		return err
	}
	fmt.Printf("harvest: %d candidates -> %s (requests %d, geo-blocked %d, pages fetched %d)\n",
		len(all), outPath, fetcher.attempts, fetcher.blocked, fetcher.fetched)
	if doIngest && len(all) > 0 {
		acc, rej := stageCandidates(cfg, db, "yahoo", all)
		fmt.Printf("ingest: %d staged, %d rejected\n", acc, rej)
	}
	return nil
}
