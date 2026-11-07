package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Mercari harvest (added 2026-11-07): discovery and detail reads for
// jp.mercari.com through the same DataImpulse proxy as the Yahoo harvest.
//
// Why this shape: Mercari search results are NOT in the search page HTML
// (client-rendered shell) - they come from the search API at
// api.mercari.jp/v2/entities:search. That API host refuses HTTP/1.1
// clients outright (flat Cloudflare 403 on every exit IP) but answers
// Go's HTTP/2 client. Anonymous search needs no account and no access
// token: each request carries a self-signed DPoP proof (ES256, ephemeral
// P-256 key, the same scheme the Mercari web client uses). All three
// facts were verified live on 2026-11-07 before this code was written.
//
// Verification parity with the retired browser+writer route: a candidate
// is staged only after its own item page has been fetched through the
// proxy and confirmed the ID (canonical link / JSON-LD offer URL - the
// writer's identity check). Item pages are requested with a crawler UA:
// Mercari serves bots the full server-rendered page whose JSON-LD block
// carries the category breadcrumb, every photo, the price (Shops
// included) and availability. The category-only rule is enforced from
// that breadcrumb exactly as on Yahoo: the path must sit inside
// テレビゲーム. Prices come from the page itself; the search API's
// price is only a fallback for Shops page variants without one.
// Koei Tecmo confirmation stays the agent step before push, exactly as
// with Yahoo: this command never sets koei_evidence.

// ---------- DPoP ----------

type dpopSigner struct {
	priv *ecdsa.PrivateKey
	jwk  string
}

func newDPoPSigner() (*dpopSigner, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pub := priv.Public().(*ecdsa.PublicKey)
	jwk := fmt.Sprintf(`{"kty":"EC","crv":"P-256","x":"%s","y":"%s"}`,
		base64.RawURLEncoding.EncodeToString(pub.X.FillBytes(make([]byte, 32))),
		base64.RawURLEncoding.EncodeToString(pub.Y.FillBytes(make([]byte, 32))))
	return &dpopSigner{priv: priv, jwk: jwk}, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func (s *dpopSigner) proof(htm, htu string) string {
	hdr := fmt.Sprintf(`{"typ":"dpop+jwt","alg":"ES256","jwk":%s}`, s.jwk)
	pl := fmt.Sprintf(`{"jti":"%s","htm":"%s","htu":"%s","iat":%d,"uuid":"%s"}`,
		randHex(16), htm, htu, time.Now().Unix(), randHex(16))
	si := base64.RawURLEncoding.EncodeToString([]byte(hdr)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(pl))
	h := sha256.Sum256([]byte(si))
	r, ss, err := ecdsa.Sign(rand.Reader, s.priv, h[:])
	if err != nil {
		return ""
	}
	sig := append(r.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
	return si + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// postSearch performs one Mercari search API call through the proxy with
// a fresh DPoP proof, retrying on 403/429/5xx with rotation (each attempt
// is a new connection, hence a new exit IP), up to 5 attempts.
func (f *proxiedFetcher) postSearch(signer *dpopSigner, body []byte) ([]byte, error) {
	const endpoint = "https://api.mercari.jp/v2/entities:search"
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		f.pace()
		f.attempts++
		req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Platform", "web")
		req.Header.Set("Origin", "https://jp.mercari.com")
		req.Header.Set("Referer", "https://jp.mercari.com/")
		req.Header.Set("DPoP", signer.proof("POST", endpoint))
		f.lastReq = time.Now()
		resp, err := f.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 200 {
			f.fetched++
			return b, nil
		}
		lastErr = fmt.Errorf("search API HTTP %d (page title n/a, body %.120s)", resp.StatusCode, string(b))
	}
	return nil, lastErr
}

// ---------- search API shapes ----------

type mercariSearchItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Price      string `json:"price"`
	Status     string `json:"status"`
	ItemType   string `json:"itemType"`
	CategoryID string `json:"categoryId"`
	Photos     []struct {
		URI string `json:"uri"`
	} `json:"photos"`
}

type mercariSearchResp struct {
	Meta struct {
		NextPageToken string `json:"nextPageToken"`
		NumFound      string `json:"numFound"`
	} `json:"meta"`
	Items []mercariSearchItem `json:"items"`
}

func mercariSearchBody(keyword, sessionID, pageToken string) []byte {
	body, _ := json.Marshal(map[string]any{
		"pageToken":       pageToken,
		"searchSessionId": sessionID,
		"indexRouting":    "INDEX_ROUTING_UNSPECIFIED",
		"searchCondition": map[string]any{
			"keyword": keyword,
			"sort":    "SORT_CREATED_TIME",
			"order":   "ORDER_DESC",
			"status":  []string{"STATUS_ON_SALE"},
		},
		"defaultDatabases": []string{},
		"serviceFrom":      "suruga",
	})
	return body
}

// ---------- matching helpers ----------

var romanNumerals = map[rune]string{
	'Ⅰ': "I", 'Ⅱ': "II", 'Ⅲ': "III", 'Ⅳ': "IV", 'Ⅴ': "V",
	'Ⅵ': "VI", 'Ⅶ': "VII", 'Ⅷ': "VIII", 'Ⅸ': "IX", 'Ⅹ': "X",
}

// normalizeTitle folds the width/case/whitespace variation sellers use,
// in the spirit of the writer's NFKC matching (Go stdlib has no NFKC;
// this covers the variation actually seen in listing titles).
func normalizeTitle(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '！' && r <= '～': // full-width ASCII
			r = r - 0xFEE0
		case r == '　':
			continue
		case unicode.IsSpace(r):
			continue
		case r == '・' || r == '〜' || r == '～':
			continue
		}
		if rep, ok := romanNumerals[r]; ok {
			b.WriteString(rep)
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// nameMatchesTitle reports whether a listing name plausibly names the
// watchlist game: the normalized name contains the full normalized JP
// title, or its main part before a subtitle separator (sellers often
// list the main title only).
func nameMatchesTitle(name, jpTitle string) bool {
	n := normalizeTitle(name)
	full := normalizeTitle(jpTitle)
	if full != "" && strings.Contains(n, full) {
		return true
	}
	main := jpTitle
	for _, sep := range []string{"：", ":", "　"} {
		if i := strings.Index(main, sep); i >= 0 {
			main = main[:i]
		}
	}
	mn := normalizeTitle(main)
	return len([]rune(mn)) >= 4 && strings.Contains(n, mn)
}

var (
	reOnlyParts   = regexp.MustCompile(`説明書のみ|箱のみ|ケースのみ|帯のみ|ジャケットのみ|付録のみ|切り抜き`)
	reMagazine    = regexp.MustCompile(`雑誌|マガジン|増刊|ムック|月号|週刊|月刊`)
	reBook        = regexp.MustCompile(`攻略本|ガイドブック|設定資料|公式ガイド|単行本`)
	reAudio       = regexp.MustCompile(`サウンドトラック|サントラ|ドラマCD`)
	reOtherPub    = regexp.MustCompile(`ナムコ|カプコン|コナミ|スクウェア|エニックス|バンダイ|ハドソン|タイトー`)
	reKoeiBrand   = regexp.MustCompile(`(?i)光栄|コーエー|KOEI`)
	softwareMarks = []string{"ソフト", "カセット", "ディスク", "ロム", "ROM", "フロッピー", "ゲーム"}
)

// relevanceGate ports sweep_write.py's gate verbatim in substance: it
// returns "" for plausible game software, else the drop reason.
func relevanceGate(title string) string {
	soft := false
	for _, m := range softwareMarks {
		if strings.Contains(title, m) {
			soft = true
			break
		}
	}
	switch {
	case reOnlyParts.MatchString(title):
		return "non-software item (partial/clipping)"
	case reMagazine.MatchString(title) && !soft:
		return "magazine issue, not game software"
	case reBook.MatchString(title) && !soft:
		return "standalone book/guide, not game software"
	case reAudio.MatchString(title) && !soft:
		return "audio product, not game software"
	case reOtherPub.MatchString(title) && !reKoeiBrand.MatchString(title):
		return "another publisher's product"
	case strings.Contains(title, "ファルコム") && !strings.Contains(title, "イース"):
		return "another publisher's product (Falcom, non-Ys)"
	}
	return ""
}

// ---------- item page parsing ----------

// mercariBotUA: Mercari serves crawler UAs the full server-rendered
// item page, including a JSON-LD block with the category breadcrumb
// and the Product data (all photos, price, availability, canonical
// offer URL). Browser UAs get a shell without them. Verified live
// 2026-11-07 for both personal items and Mercari Shops products.
const mercariBotUA = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"

var (
	reCanonical = regexp.MustCompile(`rel="canonical" href="([^"]+)"`)
	reOGTitle   = regexp.MustCompile(`property="og:title" content="([^"]*)"`)
	reOGImage   = regexp.MustCompile(`property="og:image" content="([^"]+)"`)
	rePriceMeta = regexp.MustCompile(`name="product:price:amount" content="(\d+)"`)
	reLDJSON    = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)
)

type mercariPage struct {
	Title    string
	Seller   string
	Photos   []string
	Price    int    // 0 when no price is readable anywhere on the page
	Category string // breadcrumb path, item element excluded
	Sold     bool
}

// findLDNode walks parsed JSON-LD (objects, @graph arrays, lists) and
// returns the first node whose @type equals want.
func findLDNode(v any, want string) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		if t["@type"] == want {
			return t
		}
		for _, sub := range t {
			if n := findLDNode(sub, want); n != nil {
				return n
			}
		}
	case []any:
		for _, sub := range t {
			if n := findLDNode(sub, want); n != nil {
				return n
			}
		}
	}
	return nil
}

func ldStringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := []string{}
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// parseMercariPage reads the bot-rendered item page. Identity is the
// canonical link / offer URL naming the requested ID. The category
// gate data is the page's own breadcrumb (JSON-LD BreadcrumbList),
// exactly mirroring the Yahoo breadcrumb gate: the caller requires
// the path to sit inside the video-games category.
func parseMercariPage(html []byte, eid string, shops bool) *mercariPage {
	s := string(html)
	identity := false
	if c := reCanonical.FindStringSubmatch(s); c != nil && strings.Contains(c[1], eid) {
		identity = true
	}
	p := &mercariPage{}
	var product map[string]any
	for _, m := range reLDJSON.FindAllStringSubmatch(s, -1) {
		var doc any
		if json.Unmarshal([]byte(m[1]), &doc) != nil {
			continue
		}
		if bc := findLDNode(doc, "BreadcrumbList"); bc != nil {
			if items, ok := bc["itemListElement"].([]any); ok && len(items) > 1 {
				names := []string{}
				for _, e := range items[:len(items)-1] { // last element is the item itself
					if em, ok := e.(map[string]any); ok {
						if n, ok := em["name"].(string); ok {
							names = append(names, n)
						}
					}
				}
				p.Category = strings.Join(names, " > ")
			}
		}
		if pr := findLDNode(doc, "Product"); pr != nil {
			product = pr
		}
	}
	if product != nil {
		if n, ok := product["name"].(string); ok {
			p.Title = n
		}
		p.Photos = ldStringList(product["image"])
		offers := product["offers"]
		if arr, ok := offers.([]any); ok && len(arr) > 0 {
			offers = arr[0]
		}
		if om, ok := offers.(map[string]any); ok {
			if u, ok := om["url"].(string); ok && strings.Contains(u, eid) {
				identity = true
			}
			if pr, ok := om["price"].(float64); ok {
				p.Price = int(pr)
			}
			if av, ok := om["availability"].(string); ok && strings.Contains(av, "OutOfStock") {
				p.Sold = true
			}
		}
	}
	if !identity {
		return nil
	}
	// Title fallbacks + seller: og:title carries "<title> by メルカリ"
	// (personal) or "<title> - <shop name>" (Shops).
	if og := reOGTitle.FindStringSubmatch(s); og != nil {
		ogTitle := og[1]
		if shops {
			if i := strings.LastIndex(ogTitle, " - "); i >= 0 {
				p.Seller = strings.TrimSpace(ogTitle[i+3:])
				if p.Title == "" {
					p.Title = strings.TrimSpace(ogTitle[:i])
				}
			}
		}
		if p.Title == "" {
			p.Title = strings.TrimSuffix(ogTitle, " by メルカリ")
		}
	}
	if shops {
		p.Title = strings.TrimPrefix(p.Title, "【中古】")
	}
	if p.Price == 0 {
		if pm := rePriceMeta.FindStringSubmatch(s); pm != nil {
			p.Price, _ = strconv.Atoi(pm[1])
		}
	}
	if len(p.Photos) == 0 {
		if im := reOGImage.FindStringSubmatch(s); im != nil {
			p.Photos = []string{im[1]}
		}
	}
	if p.Title == "" {
		return nil
	}
	return p
}

// inScopeMercariCategory is the category-only rule from the item
// page's own breadcrumb: Mercari puts console, handheld AND PC game
// software under テレビゲーム (verified on live pages 2026-11-07).
func inScopeMercariCategory(path string) bool {
	return strings.Contains(path, "テレビゲーム")
}

// leafVerdict records what one fetched item page's own breadcrumb
// proved about a Mercari search categoryId leaf. The map persists
// across runs (hidden_files/mercari_leaf_map.json): junk leaves
// (books, DVDs, ...) are then rejected from the search data alone,
// without spending a page fetch on every name-matching item.
type leafVerdict struct {
	Path    string `json:"path"`
	InScope bool   `json:"in_scope"`
}

func loadLeafMap(path string) map[string]leafVerdict {
	m := map[string]leafVerdict{}
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, &m)
	}
	return m
}

func saveLeafMap(path string, m map[string]leafVerdict) {
	raw, _ := json.MarshalIndent(m, "", " ")
	tmp := path + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		os.Rename(tmp, path)
	}
}

func cmdHarvestMercari(cfg *config, args []string) error {
	var titleFilter []string
	perTitle := 10
	doIngest := false
	outPath := ""
	corpusDir := homeExpand("~/workspace/koei-auctions/html/mercari")
	watchPath := homeExpand("~/workspace/koei-watchlist/watchlist.json")
	statePath := homeExpand("~/workspace/goals/koei-watchlist-monitoring/hidden_files/harvest_state_mercari.json")
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
			return fmt.Errorf("unknown harvest-mercari flag %s", args[i])
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
	known := map[string]bool{}
	if rows, err := db.Query(`SELECT auction_id FROM auctions WHERE source IN ('Mercari','Mercari Shops') AND auction_id IS NOT NULL`); err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				known[id] = true
			}
		}
		rows.Close()
	}
	if rows, err := db.Query(`SELECT external_id FROM staging WHERE source_key='mercari'`); err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				known[id] = true
			}
		}
		rows.Close()
	}
	fetcher, err := newProxiedFetcher()
	if err != nil {
		return err
	}
	signer, err := newDPoPSigner()
	if err != nil {
		return err
	}
	fmt.Printf("harvest-mercari: %d titles, proxy %s, per-title cap %d\n", len(entries), maskProxyURL(fetcher.proxy), perTitle)
	leafPath := homeExpand("~/workspace/goals/koei-watchlist-monitoring/hidden_files/mercari_leaf_map.json")
	leafMap := loadLeafMap(leafPath)
	maxFetches := perTitle * 6
	if maxFetches < 30 {
		maxFetches = 30
	}
	state := loadHarvestState(statePath)
	var all []candidate
	blockedStreak := 0
	for _, e := range entries {
		session := randHex(16)
		pageToken := ""
		scanned, numFound := 0, ""
		fetches := 0
		newCount := 0
		fatal := false
		for page := 0; page < 10 && newCount < perTitle; page++ {
			body, err := fetcher.postSearch(signer, mercariSearchBody(e.Jp, session, pageToken))
			if err != nil {
				fmt.Printf("[%s] search failed: %v\n", e.Jp, err)
				fatal = true
				break
			}
			var sr mercariSearchResp
			if err := json.Unmarshal(body, &sr); err != nil {
				fmt.Printf("[%s] search response parse failed: %v\n", e.Jp, err)
				fatal = true
				break
			}
			numFound = sr.Meta.NumFound
			for _, it := range sr.Items {
				if newCount >= perTitle {
					break
				}
				scanned++
				if it.Status != "ITEM_STATUS_ON_SALE" {
					continue
				}
				if !nameMatchesTitle(it.Name, e.Jp) {
					continue
				}
				if known[it.ID] {
					continue
				}
				if v, ok := leafMap[it.CategoryID]; ok && !v.InScope {
					continue // leaf proven non-game by an earlier page's breadcrumb
				}
				if fetches >= maxFetches {
					continue // fetch budget for this title spent; matches beyond it wait for a later run
				}
				known[it.ID] = true
				fetches++
				shops := !strings.HasPrefix(it.ID, "m")
				pageURL := "https://jp.mercari.com/item/" + it.ID
				if shops {
					pageURL = "https://jp.mercari.com/shops/product/" + it.ID
				}
				page, status, err := fetcher.fetchUA(pageURL, mercariBotUA)
				if err != nil {
					fmt.Printf("  %s: item fetch failed: %v\n", it.ID, err)
					continue
				}
				if status != 200 {
					fmt.Printf("  %s: item page HTTP %d - skipped\n", it.ID, status)
					continue
				}
				os.WriteFile(filepath.Join(corpusDir, it.ID+".html"), page, 0o644)
				parsed := parseMercariPage(page, it.ID, shops)
				if parsed == nil {
					fmt.Printf("  %s: page did not confirm ID/title - skipped\n", it.ID)
					continue
				}
				if parsed.Category != "" {
					leafMap[it.CategoryID] = leafVerdict{Path: parsed.Category, InScope: inScopeMercariCategory(parsed.Category)}
				}
				if parsed.Sold {
					fmt.Printf("  %s: sold since search - skipped\n", it.ID)
					continue
				}
				if !inScopeMercariCategory(parsed.Category) {
					fmt.Printf("  %s: out of category (%s) - dropped\n", it.ID, parsed.Category)
					continue
				}
				if reason := relevanceGate(parsed.Title); reason != "" {
					fmt.Printf("  %s: dropped by relevance gate (%s)\n", it.ID, reason)
					continue
				}
				price := parsed.Price
				if price <= 0 && shops {
					// Shops pages carry their price in the JSON-LD offer;
					// this fallback covers a page variant without it.
					price, _ = strconv.Atoi(it.Price)
				}
				if price <= 0 {
					fmt.Printf("  %s: no price readable on page - skipped\n", it.ID)
					continue
				}
				photos := parsed.Photos
				if len(photos) == 0 && len(it.Photos) > 0 {
					photos = []string{it.Photos[0].URI}
				}
				if len(photos) > 8 {
					photos = photos[:8]
				}
				srcName := "Mercari"
				if shops {
					srcName = "Mercari Shops"
				}
				all = append(all, candidate{
					ID: it.ID, Title: parsed.Title, Price: price, Photos: photos,
					Seller: parsed.Seller, GameID: e.ID, PageURL: pageURL,
					SourceName: srcName, CategoryID: it.CategoryID,
				})
				newCount++
				fmt.Printf(" %s: %s ¥%d (category %s)\n", it.ID, srcName, price, it.CategoryID)
			}
			if sr.Meta.NextPageToken == "" {
				break
			}
			pageToken = sr.Meta.NextPageToken
		}
		if fatal {
			blockedStreak++
			if blockedStreak >= 3 {
				fmt.Println("source blocked on 3 consecutive titles - stopping (honest null for the rest)")
				break
			}
			continue
		}
		blockedStreak = 0
		entry := state.Titles[e.Jp]
		entry.LastRun = time.Now().Format("2006-01-02")
		state.Titles[e.Jp] = entry
		saveHarvestState(statePath, state)
		fmt.Printf("[%s] numFound %s, scanned %d, %d new candidates\n", e.Jp, numFound, scanned, newCount)
	}
	saveLeafMap(leafPath, leafMap)
	if outPath == "" {
		outPath = homeExpand("~/workspace/goals/koei-watchlist-monitoring/hidden_files/candidates_mercari_cli_" + time.Now().Format("2006-01-02") + ".json")
	}
	rawOut, _ := json.MarshalIndent(all, "", " ")
	if err := os.WriteFile(outPath, rawOut, 0o644); err != nil {
		return err
	}
	fmt.Printf("harvest-mercari: %d candidates -> %s (requests %d, pages fetched %d)\n",
		len(all), outPath, fetcher.attempts, fetcher.fetched)
	if doIngest && len(all) > 0 {
		acc, rej := stageCandidates(cfg, db, "mercari", all)
		fmt.Printf("ingest: %d staged, %d rejected\n", acc, rej)
	}
	return nil
}
