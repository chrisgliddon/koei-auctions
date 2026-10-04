# scripts

Pipeline code for the koei-auctions asset repo. No secrets are hardcoded here —
credentials resolve at runtime (see `airtable_auth.py`).

## Files

- `airtable_auth.py` — shared Airtable API auth. Reads `AIRTABLE_API_KEY` from the
  environment first, falls back to the agent host's secure credential store.
  The key is never printed or logged.
- `thumb_pipeline.py` — watch folder → GitHub → Airtable Images.
  Usage: `thumb_pipeline.py --watch DIR` where DIR holds `<listing-id>.jpg` files.
  Copies to `thumbs/`, commits + pushes, PATCHes the Auctions `Images` field with
  the raw GitHub URL (Airtable fetches and hosts permanently), sets
  `Image Status` = "Has images".
- `html_pipeline.py` — watch folder → GitHub → Airtable HTML field.
  Usage: `html_pipeline.py --watch DIR` where DIR holds
  `<source>/<source>-<auction-id>-<YYYY-MM-DD>.html` files.
  Rejects Cloudflare challenge pages and login walls, never overwrites an
  existing fixture, appends to the `HTML` attachment field without duplicating.

## Credentials

Copy `.env.example` to `.env` (gitignored) and set `AIRTABLE_API_KEY`.
On the agent host the secure store is used automatically — no `.env` needed.

Gitleaks scans every push (`.github/workflows/gitleaks.yml`) using
`.gitleaks.toml`, which also flags Airtable PATs and credential surrogates.
