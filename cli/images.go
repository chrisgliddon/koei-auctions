package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// photoSource is one entry of the gallery board's cacheAuctionImages
// photoSources payload: the listing's ordered source photo URLs, keyed by
// its Airtable record id. The board stores them durably and mirrors the
// photos from the marketplace CDNs into its own storage.
type photoSource struct {
	AuctionID string   `json:"auctionId"`
	URLs      []string `json:"urls"`
}

// Artifact batch sizes: the cacheAuctionImages schema allows 120 listings /
// 1000 photos per call, but delivery runs in batches of 40 listings
// (Chris's <50 batch rule, 2026-11-18 — a 60-listing payload timed out).
const (
	artifactBatchListings = 40
	artifactBatchPhotos   = 400
	artifactPhotosEach    = 12
)

func ensureArtifactTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS artifact_uploads (
		record_id TEXT PRIMARY KEY,
		batch TEXT NOT NULL,
		photo_count INTEGER NOT NULL,
		prepared_at TEXT NOT NULL,
		invoked_at TEXT
	)`)
	return err
}

// recordArtifactBatch notes a prepared payload batch in artifact_uploads so
// later upload-images runs skip records already delivered to the board.
func recordArtifactBatch(db *sql.DB, batch string, sources []photoSource) error {
	now := time.Now().Format(time.RFC3339)
	for _, s := range sources {
		if _, err := db.Exec(`INSERT INTO artifact_uploads (record_id, batch, photo_count, prepared_at, invoked_at)
			VALUES (?, ?, ?, ?, NULL)
			ON CONFLICT(record_id) DO UPDATE SET batch=excluded.batch,
				photo_count=excluded.photo_count, prepared_at=excluded.prepared_at, invoked_at=NULL`,
			s.AuctionID, batch, len(s.URLs), now); err != nil {
			return err
		}
	}
	return nil
}

// writePhotoSourceBatch writes one payload batch file and returns its path.
func writePhotoSourceBatch(cfg *config, batch string, sources []photoSource) (string, error) {
	dir := filepath.Join(filepath.Dir(cfg.dbPath), "photosources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, batch+".json")
	raw, err := json.MarshalIndent(sources, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

type pushedRow struct {
	UUID     string
	External string
	RecordID string
	Photos   []string
}

// pushedWithPhotos returns every staging row already pushed to Airtable
// (state 'synced' with a record id) that carries photos, newest first.
func pushedWithPhotos(db *sql.DB) ([]pushedRow, error) {
	rows, err := db.Query(`SELECT listing_uuid, external_id, airtable_record_id,
		COALESCE(photos,'[]') FROM staging
		WHERE state='synced' AND airtable_record_id IS NOT NULL AND airtable_record_id != ''
		ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pushedRow
	for rows.Next() {
		var r pushedRow
		var photosRaw string
		if err := rows.Scan(&r.UUID, &r.External, &r.RecordID, &photosRaw); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(photosRaw), &r.Photos)
		if len(r.Photos) > 0 {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// cmdUploadImages is the combined image uploader: one flow over every
// pushed record, with --targets choosing the Airtable attachment pass,
// the artifact photoSources payload pass, or both (the default).
//
// The artifact pass prepares payload batch files; a Go binary cannot call
// artifact actions, so delivery is a two-step handoff inside the same
// flow: the calling agent invokes the board's cacheAuctionImages with
// each batch file, then runs `upload-images --mark-invoked <batch>`.
func cmdUploadImages(cfg *config, args []string) error {
	targets := "both"
	limit := 0
	force := false
	dryRun := false
	markInvoked := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--targets":
			if i+1 < len(args) {
				targets = args[i+1]
				i++
			}
		case "--limit":
			if i+1 < len(args) {
				fmt.Sscan(args[i+1], &limit)
				i++
			}
		case "--force":
			force = true
		case "--dry-run":
			dryRun = true
		case "--mark-invoked":
			if i+1 < len(args) {
				markInvoked = args[i+1]
				i++
			}
		default:
			return fmt.Errorf("unknown upload-images flag %s", args[i])
		}
	}
	wantAirtable := targets == "both" || targets == "airtable"
	wantArtifact := targets == "both" || targets == "artifact"
	if !wantAirtable && !wantArtifact {
		return fmt.Errorf("--targets must be airtable, artifact or both (got %q)", targets)
	}

	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureArtifactTable(db); err != nil {
		return err
	}

	if markInvoked != "" {
		res, err := db.Exec(`UPDATE artifact_uploads SET invoked_at=? WHERE batch=? AND invoked_at IS NULL`,
			time.Now().Format(time.RFC3339), markInvoked)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		fmt.Printf("upload-images: batch %s marked invoked (%d listings)\n", markInvoked, n)
		return nil
	}

	pushed, err := pushedWithPhotos(db)
	if err != nil {
		return err
	}
	if limit > 0 && len(pushed) > limit {
		pushed = pushed[:limit]
	}
	fmt.Printf("upload-images: %d pushed listings with photos (targets: %s)\n", len(pushed), targets)

	if wantAirtable {
		if err := uploadImagesAirtable(cfg, pushed, dryRun); err != nil {
			return err
		}
	}
	if wantArtifact {
		if err := uploadImagesArtifact(cfg, db, pushed, force, dryRun); err != nil {
			return err
		}
	}
	return nil
}

// uploadImagesAirtable ensures each pushed record's Airtable Images field
// carries its photos (URL-attach; Airtable fetches the bytes itself).
func uploadImagesAirtable(cfg *config, pushed []pushedRow, dryRun bool) error {
	cl, err := newAirtableClient(cfg)
	if err != nil {
		return err
	}
	already, attached, failed := 0, 0, 0
	for _, r := range pushed {
		rec, err := cl.get(r.RecordID)
		if err != nil {
			fmt.Printf("FAIL   %s (%s): read: %v\n", r.External, r.RecordID, err)
			failed++
			continue
		}
		if imgs, ok := rec.Fields["Images"].([]any); ok && len(imgs) > 0 {
			already++
			continue
		}
		photos := r.Photos
		if len(photos) > 8 {
			photos = photos[:8]
		}
		images := []any{}
		for _, u := range photos {
			images = append(images, map[string]any{"url": u})
		}
		if dryRun {
			fmt.Printf("DRY-RUN %s (%s): would attach %d photos\n", r.External, r.RecordID, len(images))
			attached++
			continue
		}
		if _, err := cl.do("PATCH", cfg.base+"/"+cfg.table+"/"+r.RecordID, map[string]any{
			"fields": map[string]any{"Images": images, "Image Status": "Has images"},
		}); err != nil {
			fmt.Printf("FAIL   %s (%s): attach: %v\n", r.External, r.RecordID, err)
			failed++
			continue
		}
		fmt.Printf("ATTACH %s (%s): %d photos\n", r.External, r.RecordID, len(images))
		attached++
	}
	fmt.Printf("upload-images [airtable]: %d already had images, %d attached, %d failed\n", already, attached, failed)
	if failed > 0 {
		return &exitError{code: 1}
	}
	return nil
}

// uploadImagesArtifact prepares photoSources payload batches for the
// board's cacheAuctionImages action, skipping records already delivered
// (unless --force), and records each batch in artifact_uploads.
func uploadImagesArtifact(cfg *config, db *sql.DB, pushed []pushedRow, force, dryRun bool) error {
	invoked := map[string]bool{}
	if !force {
		rows, err := db.Query(`SELECT record_id FROM artifact_uploads WHERE invoked_at IS NOT NULL`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			invoked[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	today := time.Now().Format("2006-01-02")
	// If records are already prepared under batch files from an earlier
	// run today (not yet invoked), continue numbering after them and
	// never reuse a batch id: files and artifact_uploads rows of the old
	// batches stay valid and deliverable.
	maxNo := 0
	prows, err := db.Query(`SELECT DISTINCT batch FROM artifact_uploads`)
	if err != nil {
		return err
	}
	for prows.Next() {
		var b string
		if err := prows.Scan(&b); err != nil {
			prows.Close()
			return err
		}
		if n, ok := strings.CutPrefix(b, "batch-"+today+"-"); ok {
			var v int
			if _, err := fmt.Sscan(n, &v); err == nil && v > maxNo {
				maxNo = v
			}
		}
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return err
	}
	batchNo := maxNo
	flushes := 0
	var batch []photoSource
	batchPhotos := 0
	totalListings, totalPhotos, skipped := 0, 0, 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		batchNo++
		flushes++
		batchID := fmt.Sprintf("batch-%s-%03d", today, batchNo)
		if dryRun {
			fmt.Printf("DRY-RUN %s: %d listings, %d photos\n", batchID, len(batch), batchPhotos)
		} else {
			path, err := writePhotoSourceBatch(cfg, batchID, batch)
			if err != nil {
				return err
			}
			if err := recordArtifactBatch(db, batchID, batch); err != nil {
				return err
			}
			fmt.Printf("ARTIFACT %s: %d listings, %d photos -> %s\n", batchID, len(batch), batchPhotos, path)
			fmt.Printf("         deliver with cacheAuctionImages(photoSources=<that file>), then: koei-auctions upload-images --mark-invoked %s\n", batchID)
		}
		totalListings += len(batch)
		totalPhotos += batchPhotos
		batch = nil
		batchPhotos = 0
		return nil
	}

	for _, r := range pushed {
		if invoked[r.RecordID] {
			skipped++
			continue
		}
		urls := r.Photos
		if len(urls) > artifactPhotosEach {
			urls = urls[:artifactPhotosEach]
		}
		if len(batch) > 0 && (len(batch) >= artifactBatchListings || batchPhotos+len(urls) > artifactBatchPhotos) {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, photoSource{AuctionID: r.RecordID, URLs: urls})
		batchPhotos += len(urls)
	}
	if err := flush(); err != nil {
		return err
	}
	fmt.Printf("upload-images [artifact]: %d listings, %d photos in %d batch(es); %d skipped (already delivered)\n",
		totalListings, totalPhotos, flushes, skipped)
	return nil
}

// appendPushPhotoSources records the artifact payload for one push run so
// the sweep's push step feeds the board in the same flow it feeds
// Airtable. Called by cmdPush with the records it created or found.
func appendPushPhotoSources(cfg *config, db *sql.DB, sources []photoSource) {
	if len(sources) == 0 {
		return
	}
	if err := ensureArtifactTable(db); err != nil {
		fmt.Printf("ARTIFACT payload skipped: %v\n", err)
		return
	}
	batchID := "push-" + time.Now().Format("2006-01-02-150405")
	path, err := writePhotoSourceBatch(cfg, batchID, sources)
	if err != nil {
		fmt.Printf("ARTIFACT payload skipped: %v\n", err)
		return
	}
	if err := recordArtifactBatch(db, batchID, sources); err != nil {
		fmt.Printf("ARTIFACT payload skipped: %v\n", err)
		return
	}
	fmt.Printf("ARTIFACT payload: %d listings -> %s (deliver with cacheAuctionImages, then upload-images --mark-invoked %s)\n",
		len(sources), path, batchID)
}
