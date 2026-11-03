package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

func sourceFamily(sourceKey string) map[string]bool {
	switch sourceKey {
	case "yahoo":
		return map[string]bool{"Yahoo": true, "PayPay Flea": true}
	case "mercari":
		return map[string]bool{"Mercari": true, "Mercari Shops": true}
	case "surugaya":
		return map[string]bool{"Surugaya": true}
	}
	return map[string]bool{}
}

func parseSourceFlag(args []string) (string, []string, error) {
	src := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--source" && i+1 < len(args) {
			src = args[i+1]
			i++
		} else {
			rest = append(rest, args[i])
		}
	}
	if src != "yahoo" && src != "mercari" && src != "surugaya" {
		return "", nil, fmt.Errorf("--source must be yahoo|mercari|surugaya")
	}
	return src, rest, nil
}

func loadCandidateFile(path string) ([]candidate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []candidate
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse candidates: %w", err)
	}
	return out, nil
}

func cmdSync(cfg *config) error {
	cl, err := newAirtableClient(cfg)
	if err != nil {
		return err
	}
	recs, err := cl.listAll()
	if err != nil {
		return err
	}
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := rebuildIndex(db, recs); err != nil {
		return err
	}
	var dupUUID, dupPair, noUUID int
	db.QueryRow(`SELECT COUNT(*) FROM (SELECT listing_uuid FROM auctions
		WHERE listing_uuid IS NOT NULL GROUP BY listing_uuid HAVING COUNT(*)>1)`).Scan(&dupUUID)
	db.QueryRow(`SELECT COUNT(*) FROM (SELECT auction_id, source FROM auctions
		WHERE auction_id IS NOT NULL GROUP BY auction_id, source HAVING COUNT(*)>1)`).Scan(&dupPair)
	db.QueryRow(`SELECT COUNT(*) FROM auctions WHERE listing_uuid IS NULL`).Scan(&noUUID)
	fmt.Printf("index rebuilt: %d auctions | without UUID: %d | duplicate UUIDs: %d | duplicate (ID,Source): %d\n",
		len(recs), noUUID, dupUUID, dupPair)
	return nil
}

func cmdStatus(cfg *config) error {
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	count, built, err := indexMeta(db)
	if err != nil {
		return err
	}
	age := ""
	if d, err := indexAge(db); err == nil {
		age = fmt.Sprintf(" (%.1fh old)", d.Hours())
	}
	fmt.Printf("auctions index: %s records, built %s%s\n", count, built, age)
	rows, err := db.Query(`SELECT state, COUNT(*) FROM staging GROUP BY state ORDER BY state`)
	if err != nil {
		return err
	}
	defer rows.Close()
	any := false
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err == nil {
			fmt.Printf("staging %-8s %d\n", st, n)
			any = true
		}
	}
	if !any {
		fmt.Println("staging: empty")
	}
	return rows.Err()
}

// lintOne checks a single external ID against the auctions index.
// Returns the number of hits (0 = clean) and prints one line per result.
func lintOne(cfg *config, db *sql.DB, sourceKey, eid string) int {
	u := cfg.listingUUID(sourceKey, eid)
	var recID, src, title string
	err := db.QueryRow(`SELECT record_id, source, COALESCE(title,'') FROM auctions WHERE listing_uuid=?`, u).
		Scan(&recID, &src, &title)
	if err == nil {
		fmt.Printf("DUPLICATE     %s: already in base as %s [%s] %q\n", eid, recID, src, trunc(title, 60))
		return 1
	}
	rows, err := db.Query(`SELECT record_id, listing_uuid, source, COALESCE(title,'') FROM auctions WHERE auction_id=?`, eid)
	if err != nil {
		fmt.Println("lint query:", err)
		return 1
	}
	defer rows.Close()
	hits := 0
	fam := sourceFamily(sourceKey)
	for rows.Next() {
		var rid, lu, rsrc, rtitle string
		if err := rows.Scan(&rid, &lu, &rsrc, &rtitle); err != nil {
			continue
		}
		hits++
		if fam[rsrc] {
			fmt.Printf("UUID-MISMATCH %s: same ID+source exists as %s with a different Listing UUID (%s) - adjudicate before writing\n", eid, rid, lu)
		} else {
			fmt.Printf("CROSS-SOURCE  %s: ID exists under source %s as %s %q - check it is really a different listing\n", eid, rsrc, rid, trunc(rtitle, 60))
		}
	}
	if hits == 0 {
		fmt.Printf("ok            %s: not in base\n", eid)
	}
	return hits
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func cmdLint(cfg *config, args []string) error {
	src, rest, err := parseSourceFlag(args)
	if err != nil {
		return err
	}
	var ids []string
	for i := 0; i < len(rest); i++ {
		if rest[i] == "--ids" && i+1 < len(rest) {
			for _, x := range strings.Split(rest[i+1], ",") {
				if x = strings.TrimSpace(x); x != "" {
					ids = append(ids, x)
				}
			}
			i++
		} else {
			cands, err := loadCandidateFile(rest[i])
			if err != nil {
				return err
			}
			for _, c := range cands {
				if c.ID != "" {
					ids = append(ids, c.ID)
				}
			}
		}
	}
	if len(ids) == 0 {
		return fmt.Errorf("no candidate IDs given")
	}
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	count, built, _ := indexMeta(db)
	fmt.Printf("index: %s auctions, built %s\n", count, built)
	if d, err := indexAge(db); err == nil && d > 24*time.Hour {
		fmt.Println("WARNING: index is over 24h old - rebuild (sync) before trusting a clean result")
	}
	hits := 0
	for _, id := range ids {
		hits += lintOne(cfg, db, src, id)
	}
	fmt.Printf("%d checked, %d hit(s)\n", len(ids), hits)
	if hits > 0 {
		return &exitError{code: 1}
	}
	return nil
}

func cmdIngest(cfg *config, args []string) error {
	src, rest, err := parseSourceFlag(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("ingest needs exactly one candidates file")
	}
	cands, err := loadCandidateFile(rest[0])
	if err != nil {
		return err
	}
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	accepted, rejected := 0, 0
	seen := map[string]bool{}
	for _, c := range cands {
		if c.ID == "" {
			continue
		}
		u := cfg.listingUUID(src, c.ID)
		if seen[u] {
			fmt.Printf("REJECT %s: duplicate inside the candidates file\n", c.ID)
			rejected++
			continue
		}
		seen[u] = true
		if n := lintOne(cfg, db, src, c.ID); n > 0 {
			rejected++
			continue
		}
		var stagedCount int
		db.QueryRow(`SELECT COUNT(*) FROM staging WHERE listing_uuid=?`, u).Scan(&stagedCount)
		if stagedCount > 0 {
			fmt.Printf("REJECT %s: already staged (%s)\n", c.ID, u)
			rejected++
			continue
		}
		display := c.SourceName
		if display == "" {
			display = sourceDisplay(src, c.ID)
		}
		photos, _ := json.Marshal(c.Photos)
		var bids any
		if c.Bids != nil {
			bids = *c.Bids
		}
		_, err := db.Exec(`INSERT INTO staging
			(listing_uuid, source_key, source_name, external_id, title, price, seller, bids,
			 ends_at, photos, release_id, state, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,'staged',?,?)`,
			u, src, display, c.ID, nullable(c.Title), c.Price, nullable(c.Seller), bids,
			nullable(c.EndsAt), string(photos), nullable(c.ReleaseID), now, now)
		if err != nil {
			fmt.Printf("REJECT %s: insert failed: %v\n", c.ID, err)
			rejected++
			continue
		}
		fmt.Printf("STAGED %s: %s %q ¥%d\n", c.ID, u, trunc(c.Title, 50), c.Price)
		accepted++
	}
	fmt.Printf("ingest: %d staged, %d rejected of %d candidates\n", accepted, rejected, len(cands))
	return nil
}

type stagedRow struct {
	UUID       string
	SourceKey  string
	SourceName string
	ExternalID string
	Title      string
	Price      int
	Seller     sql.NullString
	Bids       sql.NullInt64
	EndsAt     sql.NullString
	Photos     string
	ReleaseID  sql.NullString
	CreatedAt  string
}

func cmdPush(cfg *config, args []string) error {
	dryRun, allowUnlinked := false, false
	limit := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			dryRun = true
		case "--allow-unlinked":
			allowUnlinked = true
		case "--limit":
			if i+1 < len(args) {
				fmt.Sscan(args[i+1], &limit)
				i++
			}
		default:
			return fmt.Errorf("unknown push flag %s", args[i])
		}
	}
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT listing_uuid, source_key, source_name, external_id,
		COALESCE(title,''), COALESCE(price,0), seller, bids, ends_at, COALESCE(photos,'[]'),
		release_id, created_at FROM staging WHERE state='staged' ORDER BY created_at`)
	if err != nil {
		return err
	}
	var staged []stagedRow
	for rows.Next() {
		var r stagedRow
		if err := rows.Scan(&r.UUID, &r.SourceKey, &r.SourceName, &r.ExternalID, &r.Title,
			&r.Price, &r.Seller, &r.Bids, &r.EndsAt, &r.Photos, &r.ReleaseID, &r.CreatedAt); err != nil {
			return err
		}
		staged = append(staged, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	cl, err := newAirtableClient(cfg)
	if err != nil {
		return err
	}
	today := time.Now().Format("2006-01-02")
	pushed, held, already, failed := 0, 0, 0, 0
	for _, r := range staged {
		if limit > 0 && pushed+already >= limit {
			break
		}
		if !r.ReleaseID.Valid && !allowUnlinked {
			fmt.Printf("HELD   %s: no release_id staged (push with --allow-unlinked to override)\n", r.ExternalID)
			held++
			continue
		}
		existing, err := cl.findByUUID(r.UUID)
		if err != nil {
			fmt.Printf("FAIL   %s: live check: %v\n", r.ExternalID, err)
			failed++
			continue
		}
		if existing != nil {
			fmt.Printf("EXISTS %s: already in Airtable as %s - marking synced, no create\n", r.ExternalID, existing.ID)
			if !dryRun {
				markSynced(db, r.UUID, existing.ID, "already in Airtable at push time (live UUID check)")
				upsertIndexRow(db, existing)
			}
			already++
			continue
		}
		var photos []string
		json.Unmarshal([]byte(r.Photos), &photos)
		if len(photos) > 8 {
			photos = photos[:8]
		}
		images := []any{}
		for _, u := range photos {
			images = append(images, map[string]any{"url": u})
		}
		via := "Jauce"
		if r.SourceKey == "mercari" {
			via = "Mercari"
		}
		notes := fmt.Sprintf("Found via %s on %s. Active/buyable at time of check. Evidence: detail page read ingested into SQLite on %s; pushed by koei-auctions CLI on %s.",
			via, today, r.CreatedAt[:10], today)
		fields := map[string]any{
			"Auction ID":   r.ExternalID,
			"Listing UUID": r.UUID,
			"Item Title":   r.Title,
			"Source":       r.SourceName,
			"Price":        r.Price,
			"FX":           []string{fxRecordID},
			"Images":       images,
			"Image Status": map[bool]string{true: "Has images", false: "No images in listing"}[len(photos) > 0],
			"Date Scraped": today,
			"Listing Type": map[bool]string{true: "Auction", false: "Buy It Now"}[r.SourceKey == "yahoo" || r.SourceKey == "surugaya"],
			"Notes":        notes,
		}
		if u := canonicalURL(r.SourceKey, r.ExternalID); u != "" {
			fields["URL"] = u
		}
		if r.ReleaseID.Valid {
			fields["Game Release"] = []string{r.ReleaseID.String}
		}
		if r.Seller.Valid && r.Seller.String != "" {
			fields["Seller"] = r.Seller.String
		}
		if r.Bids.Valid {
			fields["Number of Bids"] = int(r.Bids.Int64)
		}
		if r.EndsAt.Valid && r.EndsAt.String != "" {
			fields["Ends At"] = r.EndsAt.String
		}
		if dryRun {
			pretty, _ := json.MarshalIndent(fields, "", "  ")
			fmt.Printf("DRY-RUN %s: would create:\n%s\n", r.ExternalID, pretty)
			pushed++
			continue
		}
		recID, err := cl.create(fields)
		if err != nil {
			fmt.Printf("FAIL   %s: create: %v\n", r.ExternalID, err)
			failed++
			continue
		}
		back, err := cl.get(recID)
		if err != nil {
			fmt.Printf("FAIL   %s: created %s but read-back failed: %v\n", r.ExternalID, recID, err)
			failed++
			continue
		}
		if strField(back.Fields, "Listing UUID") != r.UUID {
			fmt.Printf("FAIL   %s: read-back UUID mismatch on %s\n", r.ExternalID, recID)
			failed++
			continue
		}
		if fx, ok := back.Fields["FX"].([]any); !ok || len(fx) == 0 {
			fmt.Printf("FAIL   %s: read-back missing FX link on %s\n", r.ExternalID, recID)
			failed++
			continue
		}
		if r.ReleaseID.Valid {
			links, _ := back.Fields["Game Release"].([]any)
			found := false
			for _, l := range links {
				if l == r.ReleaseID.String {
					found = true
				}
			}
			if !found {
				fmt.Printf("FAIL   %s: read-back missing Release link on %s\n", r.ExternalID, recID)
				failed++
				continue
			}
		}
		markSynced(db, r.UUID, recID, "")
		upsertIndexRow(db, back)
		fmt.Printf("PUSHED %s: created %s and verified (UUID, FX%s)\n", r.ExternalID, recID,
			map[bool]string{true: ", Release", false: ""}[r.ReleaseID.Valid])
		pushed++
	}
	fmt.Printf("push: %d pushed, %d already in Airtable, %d held, %d failed (of %d staged)\n",
		pushed, already, held, failed, len(staged))
	if failed > 0 {
		return &exitError{code: 1}
	}
	return nil
}

func markSynced(db *sql.DB, uuid, recID, note string) {
	db.Exec(`UPDATE staging SET state='synced', airtable_record_id=?, note=?, updated_at=? WHERE listing_uuid=?`,
		recID, nullable(note), time.Now().UTC().Format(time.RFC3339), uuid)
}
