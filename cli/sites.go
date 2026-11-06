package main

import "strings"

// Proxy / storefront sites that address a Yahoo Auctions listing by its
// auction ID alone. The auction ID is the universal key: every registered
// site's item URL is its template with {id} replaced by the ID. The same
// templates are mirrored as formula fields in the Airtable Auctions table,
// so a record only needs Source + Auction ID for every link to exist.
//
// Patterns verified 2026-11-05:
//   - yahoo:     the canonical listing URL (also the CLI's fetch host).
//   - jauce:     long-standing pattern, used by this project since 2025-09.
//   - zenmarket: long-standing pattern (itemCode == auction ID exactly).
//     Registered for URL generation only — fetching through ZenMarket is
//     banned (Chris's directive, 2026-11-05).
//   - buyee:     /item/yahoo/auction/<id> redirects to the canonical
//     /item/jdirectitems/auction/<id> form registered here (live-checked).
//   - fromjapan: live-checked HTTP 200; multiple real links in the wild.
//   - neokyo:    real indexed item pages show the Auction ID at this path.
type site struct {
	Key      string
	Name     string
	Template string // contains exactly one {id}
}

var sites = []site{
	{"yahoo", "Yahoo Auctions", "https://auctions.yahoo.co.jp/jp/auction/{id}"},
	{"jauce", "Jauce", "https://www.jauce.com/auction/{id}"},
	{"zenmarket", "ZenMarket", "https://zenmarket.jp/auction.aspx?itemCode={id}"},
	{"buyee", "Buyee", "https://buyee.jp/item/jdirectitems/auction/{id}"},
	{"fromjapan", "FromJapan", "https://www.fromjapan.co.jp/japan/en/auction/yahoo/input/{id}/"},
	{"neokyo", "Neokyo", "https://neokyo.com/en/product/yahoo/{id}"},
}

// siteByKey finds a registered site.
func siteByKey(key string) (site, bool) {
	for _, s := range sites {
		if s.Key == key {
			return s, true
		}
	}
	return site{}, false
}

// siteURL builds one site's item URL for a Yahoo auction ID. z-prefixed IDs
// are PayPay Flea Market item IDs, not Yahoo auction IDs — no proxy pattern
// applies, so it returns "".
func siteURL(key, auctionID string) string {
	if auctionID == "" || strings.HasPrefix(auctionID, "z") {
		return ""
	}
	s, ok := siteByKey(key)
	if !ok {
		return ""
	}
	return strings.Replace(s.Template, "{id}", auctionID, 1)
}
