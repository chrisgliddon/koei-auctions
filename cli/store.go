package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

func openDB(cfg *config) (*sql.DB, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", cfg.dbPath))
	if err != nil {
		return nil, err
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS auctions (
			record_id TEXT PRIMARY KEY,
			auction_id TEXT,
			listing_uuid TEXT,
			source TEXT,
			url TEXT,
			title TEXT,
			date_scraped TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_uuid ON auctions(listing_uuid)`,
		`CREATE INDEX IF NOT EXISTS idx_aid ON auctions(auction_id)`,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE IF NOT EXISTS staging (
			listing_uuid TEXT PRIMARY KEY,
			source_key TEXT NOT NULL,
			source_name TEXT NOT NULL,
			external_id TEXT NOT NULL,
			title TEXT,
			price INTEGER,
			seller TEXT,
			bids INTEGER,
			ends_at TEXT,
			photos TEXT,
			release_id TEXT,
			state TEXT NOT NULL DEFAULT 'staged',
			airtable_record_id TEXT,
			note TEXT,
			created_at TEXT,
			updated_at TEXT
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	return db, nil
}

// rebuildIndex replaces the auctions mirror table from a full Airtable pull.
// The staging table is left untouched.
func rebuildIndex(db *sql.DB, recs []atRecord) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DROP TABLE IF EXISTS auctions`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE auctions (
		record_id TEXT PRIMARY KEY, auction_id TEXT, listing_uuid TEXT,
		source TEXT, url TEXT, title TEXT, date_scraped TEXT)`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO auctions VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	for _, r := range recs {
		ds := strField(r.Fields, "Date Scraped")
		if len(ds) > 10 {
			ds = ds[:10]
		}
		if _, err := stmt.Exec(r.ID,
			nullable(strField(r.Fields, "Auction ID")),
			nullable(strField(r.Fields, "Listing UUID")),
			nullable(strField(r.Fields, "Source")),
			nullable(strField(r.Fields, "URL")),
			nullable(strField(r.Fields, "Item Title")),
			nullable(ds)); err != nil {
			return err
		}
	}
	stmt.Close()
	if _, err := tx.Exec(`CREATE INDEX idx_uuid ON auctions(listing_uuid)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX idx_aid ON auctions(auction_id)`); err != nil {
		return err
	}
	built := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('built_at',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, built); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('count',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(len(recs))); err != nil {
		return err
	}
	return tx.Commit()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func indexMeta(db *sql.DB) (count string, builtAt string, err error) {
	rows, err := db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			m[k] = v
		}
	}
	return m["count"], m["built_at"], rows.Err()
}

func indexAge(db *sql.DB) (time.Duration, error) {
	_, built, err := indexMeta(db)
	if err != nil || built == "" {
		return 0, err
	}
	t, err := time.Parse(time.RFC3339, built)
	if err != nil {
		return 0, err
	}
	return time.Since(t), nil
}

// upsertIndexRow adds one freshly pushed record to the local mirror so
// subsequent lint/ingest runs in the same session see it immediately.
func upsertIndexRow(db *sql.DB, r *atRecord) error {
	ds := strField(r.Fields, "Date Scraped")
	if len(ds) > 10 {
		ds = ds[:10]
	}
	_, err := db.Exec(`INSERT INTO auctions VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(record_id) DO UPDATE SET auction_id=excluded.auction_id,
		listing_uuid=excluded.listing_uuid, source=excluded.source,
		url=excluded.url, title=excluded.title, date_scraped=excluded.date_scraped`,
		r.ID, nullable(strField(r.Fields, "Auction ID")), nullable(strField(r.Fields, "Listing UUID")),
		nullable(strField(r.Fields, "Source")), nullable(strField(r.Fields, "URL")),
		nullable(strField(r.Fields, "Item Title")), nullable(ds))
	return err
}
