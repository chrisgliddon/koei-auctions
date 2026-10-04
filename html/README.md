# HTML fixtures

Archived source HTML for auction listings, captured at sweep time.

Layout: `html/<source>/<auction-id>-<YYYY-MM-DD>.html`
- `source`: yahoo | mercari | surugaya
- Never overwrite a valid prior archive; new captures get a new date suffix.
- Challenge/login pages are never saved — only real listing HTML.

Attached to the Airtable Auctions `HTML` field via raw GitHub URLs;
Airtable fetches and hosts them permanently.
