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

## Usage

```
koei-auctions sync
koei-auctions status
koei-auctions lint   --source yahoo candidates.json
koei-auctions lint   --source mercari --ids m12345678901,m23456789012
koei-auctions ingest --source yahoo candidates.json
koei-auctions push [--dry-run] [--allow-unlinked] [--limit N]
```

Source keys are the UUID source keys: `yahoo` (Jauce/Buyee/PayPay Flea),
`mercari` (personal + Shops), `surugaya`.

Candidates are the sweep's JSON shape (`id`, `title`, `price`, `photos`,
`seller`, `bids`, `ends_at`, `page_url`), plus an optional adjudicated
`release_id` (Airtable Release record ID) that push writes as the
Game Release link.
