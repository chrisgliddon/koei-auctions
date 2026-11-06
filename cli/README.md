# koei-auctions CLI

Staging pipeline for the Koei Auctions Airtable base (`appMtKWq2FbIe2HTn`).
Records go **into SQLite first, then to Airtable** — the local store is the
gate that keeps duplicates and unverified listings out of the base.

## Pipeline

1. **ingest** — sweep/browser candidate files (JSON) are staged into the
   `staging` table. Every candidate gets its deterministic Listing UUID
   (UUID v5 over `<source>:<external_id>`, namespace in the goal's
   `uuid_namespace.txt`) and is checked against the auctions index and the
   staging table itself. Duplicates are rejected at the door.
2. **lint** — the same checks as a standalone pre-flight for any candidate
   file or ID list: `DUPLICATE` (UUID match), `CROSS-SOURCE` (same ID under
   another storefront), `UUID-MISMATCH` (same ID+source, different UUID —
   a base inconsistency). Exit 1 on any hit.
   **Koei Tecmo confirmation gate (standing rule, 2026-11-02):** a candidate
   is only staged for pushing when it carries a `koei_evidence` string —
   what in the listing's own photos or publisher info confirms it is a
   Koei Tecmo product (branding, KOE- catalog code, imprint). Push **holds**
   any staged row without one (`--allow-unconfirmed` overrides, for
   adjudicated cases only). The confirmation text is written into the
   created record's Notes.
3. **push** — staged rows are created in Airtable one at a time. Before
   creating, each UUID is live-checked in Airtable (a hit marks the row
   synced without creating anything). After creating, the record is read
   back and the push only counts when the UUID, FX link and Release link
   verify. Rows without a `release_id` are **held** unless
   `--allow-unlinked` is given. Pushed rows are also upserted into the
   local index so later lint/ingest runs see them immediately.
4. **sync** — rebuilds the `auctions` mirror table from a full Airtable
   pull (the staging table is untouched). Run it whenever the index age
   warning appears (>24h).

## The store

One shared SQLite file serves this CLI and the Python tools
(`build_auctions_index.py` / `lint_auctions.py` in the goal's hidden_files):

- default path: `~/workspace/goals/koei-watchlist-monitoring/hidden_files/auctions_index.sqlite`
- override with `KOEI_AUCTIONS_DB`
- tables: `auctions` (mirror), `meta` (built_at/count), `staging` (queue)

The database file is data, not code — it is never committed.

## Auth

No API key in the binary, the repo, or the environment. Like the Python
CLI, the binary asks the runtime's authd (unix socket
`/run/hatch/auth/authd.sock`, override `JARVIS_AUTHD_SOCK`) for the
`custom.airtable` surrogate and sends it as a Bearer token; the egress
proxy exchanges the surrogate for the real credential.

## Harvest (Yahoo Auctions via DataImpulse)

`harvest` is the discovery/fetch layer, modeled on the video-fetch CLI:

- All fetching goes through the DataImpulse proxy (`BUNKO_PROXY_URL` from
  `~/.config/bunko/proxy.env`) — **never direct, never ZenMarket**. Yahoo
  record URLs are `https://auctions.yahoo.co.jp/jp/auction/<id>`.
- The gateway rotates the residential exit IP per connection (keep-alives
  off). Roughly half of exit IPs are EEA/UK-geolocated and Yahoo geo-blocks
  those with a 403 page, so fetches retry on fresh connections (bounded).
- For each watchlist title: proxied search (IDs only — grid data is never
  evidence), then each unknown ID's item page is fetched, **saved to the
  local corpus** (`~/workspace/koei-auctions/html/yahoo/<id>.html`), and
  parsed from its `__NEXT_DATA__` payload. The parsed `auctionId` must
  equal the requested ID or the page is discarded (identity check), and
  the item's own category breadcrumb must be a game/computer category or
  the candidate is dropped (category-only rule).
- Candidates land in the candidates JSON and, with `--ingest`, in the
  staging table. Harvest never sets `koei_evidence` — an agent confirms
  Koei Tecmo branding from the saved photos and records it with
  `confirm`, after which `push` releases the row.
- Progress is resumable: `harvest_state.json` (per-title last run + seen
  IDs) is saved atomically after every title, and known IDs (index +
  staging) are skipped before fetching.

## Proxy sites (the auction ID is the key)

Every Yahoo Auctions proxy/storefront addresses a listing by its auction
ID alone. The CLI registers each verified pattern (`sites` lists them;
`sites <auction-id>` prints every URL for an ID):

| key | pattern |
|---|---|
| yahoo | `https://auctions.yahoo.co.jp/jp/auction/{id}` |
| jauce | `https://www.jauce.com/auction/{id}` |
| zenmarket | `https://zenmarket.jp/auction.aspx?itemCode={id}` |
| buyee | `https://buyee.jp/item/jdirectitems/auction/{id}` |
| fromjapan | `https://www.fromjapan.co.jp/japan/en/auction/yahoo/input/{id}/` |
| neokyo | `https://neokyo.com/en/product/yahoo/{id}` |
| dejapan | `https://www.dejapan.com/en/auction/yahoo/item/x/{id}` (slug is decorative) |
| rinkya | `https://www.rinkya.com/en/auction-{id}` |
| doorzo | hex-encoded Yahoo URL in the path — CLI only, no Airtable formula |

The same patterns exist as formula fields in the Airtable Auctions table
(Jauce URL, ZenMarket URL, Buyee URL, FromJapan URL, Neokyo URL, DeJAPAN URL, Rinkya URL), and the
Canonical URL formula's Yahoo branch is the direct Yahoo URL — so a record
needs only Source + Auction ID for all of its links to exist. ZenMarket is
registered for URL generation only; fetching through it stays banned.
z-prefixed IDs are PayPay Flea Market items, not Yahoo auctions, and get
no proxy URLs.

## Usage

```
koei-auctions sync
koei-auctions status
koei-auctions lint   --source yahoo candidates.json
koei-auctions lint   --source mercari --ids m12345678901,m23456789012
koei-auctions ingest --source yahoo candidates.json
koei-auctions push [--dry-run] [--allow-unlinked] [--allow-unconfirmed] [--limit N]
```

Source keys are the UUID source keys: `yahoo` (Jauce/Buyee/PayPay Flea),
`mercari` (personal + Shops), `surugaya`.

Candidates are the sweep's JSON shape (`id`, `title`, `price`, `photos`,
`seller`, `bids`, `ends_at`, `page_url`), plus an optional adjudicated
`release_id` (Airtable Release record ID) that push writes as the
Game Release link, and the `koei_evidence` confirmation string described
above.
