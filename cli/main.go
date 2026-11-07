// Command koei-auctions is the staging pipeline for the Koei Auctions
// Airtable base: listings are ingested into a local SQLite store first
// (dedupe by deterministic Listing UUID), linted there, and only then
// pushed to Airtable. The SQLite file is the shared store also used by
// the Python linter (build_auctions_index.py / lint_auctions.py).
//
// Airtable auth uses the runtime's authd surrogate: the binary fetches a
// short-lived hsurr: token over the authd unix socket and never sees or
// stores the real API key.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultBase  = "appMtKWq2FbIe2HTn"
	defaultTable = "tbl0jGTbqhC6sRFhW"
	fxRecordID   = "recKdV75yztonokCL"
)

type config struct {
	dbPath    string
	nsFile    string
	base      string
	table     string
	namespace [16]byte
}

func homeExpand(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func loadConfig() (*config, error) {
	c := &config{
		dbPath: os.Getenv("KOEI_AUCTIONS_DB"),
		nsFile: os.Getenv("KOEI_UUID_NS_FILE"),
		base:   defaultBase,
		table:  defaultTable,
	}
	if c.dbPath == "" {
		c.dbPath = "~/workspace/goals/koei-watchlist-monitoring/hidden_files/auctions_index.sqlite"
	}
	if c.nsFile == "" {
		c.nsFile = "~/workspace/goals/koei-watchlist-monitoring/uuid_namespace.txt"
	}
	c.dbPath = homeExpand(c.dbPath)
	c.nsFile = homeExpand(c.nsFile)
	raw, err := os.ReadFile(c.nsFile)
	if err != nil {
		return nil, fmt.Errorf("read UUID namespace %s: %w", c.nsFile, err)
	}
	ns, err := parseUUID(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("parse UUID namespace: %w", err)
	}
	c.namespace = ns
	return c, nil
}

func parseUUID(s string) ([16]byte, error) {
	var b [16]byte
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 {
		return b, fmt.Errorf("bad uuid %q", s)
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return b, err
	}
	copy(b[:], raw)
	return b, nil
}

// listingUUID is the base's dedupe key: UUID v5 over "<source>:<external_id>"
// with the external ID lowercased (Jauce/Buyee/PayPay all key as "yahoo").
func (c *config) listingUUID(sourceKey, externalID string) string {
	name := sourceKey + ":" + strings.ToLower(strings.TrimSpace(externalID))
	h := sha1.New()
	h.Write(c.namespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x50 // version 5
	sum[8] = (sum[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// sourceDisplay maps a UUID source key (+ listing ID shape) to the Source
// select value used in the base, mirroring sweep_write.py conventions.
func sourceDisplay(sourceKey, externalID string) string {
	switch sourceKey {
	case "yahoo":
		if strings.HasPrefix(externalID, "z") {
			return "PayPay Flea"
		}
		return "Yahoo"
	case "mercari":
		if strings.HasPrefix(externalID, "m") {
			return "Mercari"
		}
		return "Mercari Shops"
	case "surugaya":
		return "Surugaya"
	}
	return sourceKey
}

func canonicalURL(sourceKey, externalID string) string {
	switch sourceDisplay(sourceKey, externalID) {
	case "Yahoo":
		return siteURL("yahoo", externalID)
	case "PayPay Flea":
		return "https://paypayfleamarket.yahoo.co.jp/item/" + externalID
	case "Mercari":
		return "https://jp.mercari.com/item/" + externalID
	case "Mercari Shops":
		return "https://jp.mercari.com/shops/product/" + externalID
	}
	return ""
}

type candidate struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Price        int      `json:"price"`
	Photos       []string `json:"photos"`
	Seller       string   `json:"seller"`
	Bids         *int     `json:"bids"`
	EndsAt       string   `json:"ends_at"`
	ReleaseID    string   `json:"release_id"`
	SourceName   string   `json:"source_name"`
	CategoryID   string   `json:"category_id"`
	KoeiEvidence string   `json:"koei_evidence"`
	GameID       string   `json:"game_id"`
	PageURL      string   `json:"page_url"`
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: koei-auctions <command> [options]

commands:
  sync                                  rebuild the SQLite auctions index from Airtable
  status                                index + staging counts
  lint   --source KEY (file | --ids a,b)  check candidates against the index (exit 1 on hits)
  ingest --source KEY <candidates.json> stage candidates into SQLite (dedupe on ingest)
  push   [--dry-run] [--allow-unlinked] [--allow-unconfirmed] [--limit N]
                                        push staged records to Airtable, verify by read-back
  sites [<auction-id>]                list registered proxy sites, or print every site's URL for an ID
  confirm --id X --evidence "..." [--release recId]
                                        record the agent's Koei confirmation on a staged row
  harvest [--titles a,b] [--per-title N] [--ingest] [--out file]
                                        fetch Yahoo Auctions via the DataImpulse proxy
                                        (search + item pages), save pages to the local
                                        corpus, parse and stage candidates
  harvest-mercari [--titles a,b] [--per-title N] [--ingest] [--out file]
                                        fetch Mercari via the DataImpulse proxy
                                        (search API with DPoP proofs + item pages),
                                        save pages to the local corpus, parse and
                                        stage candidates

source KEY is the UUID source key: yahoo | mercari | surugaya
env: KOEI_AUCTIONS_DB, KOEI_UUID_NS_FILE, JARVIS_AUTHD_SOCK`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	var runErr error
	switch os.Args[1] {
	case "sync":
		runErr = cmdSync(cfg)
	case "status":
		runErr = cmdStatus(cfg)
	case "lint":
		runErr = cmdLint(cfg, os.Args[2:])
	case "ingest":
		runErr = cmdIngest(cfg, os.Args[2:])
	case "push":
		runErr = cmdPush(cfg, os.Args[2:])
	case "harvest":
		runErr = cmdHarvest(cfg, os.Args[2:])
	case "harvest-mercari":
		runErr = cmdHarvestMercari(cfg, os.Args[2:])
	case "confirm":
		runErr = cmdConfirm(cfg, os.Args[2:])
	case "sites":
		runErr = cmdSites(cfg, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if runErr != nil {
		if ee, ok := runErr.(*exitError); ok {
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "error:", runErr)
		os.Exit(1)
	}
}

type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }
