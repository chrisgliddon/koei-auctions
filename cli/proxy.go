package main

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Proxy resolution mirrors the video-fetch CLI: "auto" reads BUNKO_PROXY_URL
// from ~/.config/bunko/proxy.env (DataImpulse, persisted 2026-10-04). The
// credential value is never printed; only the masked form is shown.

func readProxyEnvFile(path, key string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+"=") {
			return strings.Trim(strings.TrimSpace(line[len(key)+1:]), `"'`)
		}
	}
	return ""
}

func resolveFetchProxy() (*url.URL, error) {
	v := readProxyEnvFile(filepath.Join(os.Getenv("HOME"), ".config/bunko/proxy.env"), "BUNKO_PROXY_URL")
	if v == "" {
		return nil, fmt.Errorf("no BUNKO_PROXY_URL in ~/.config/bunko/proxy.env - the harvest fetches only through the proxy")
	}
	return url.Parse(v)
}

func maskProxyURL(u *url.URL) string {
	if u == nil {
		return "(none)"
	}
	if u.User != nil {
		return fmt.Sprintf("%s://***@%s", u.Scheme, u.Host)
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}

// proxiedFetcher fetches pages through the DataImpulse gateway. Keep-alives
// are disabled so every request opens a fresh connection and the gateway
// rotates the residential exit IP per request. A share of exit IPs are
// EEA/UK-geolocated and Yahoo answers those with its EEA/UK notice page
// (HTTP 403) - but a bare 403 is NEVER assumed to be that page: the fetcher
// classifies every blocked response from its actual body (see isGeoBlockPage)
// so reports state exactly what came back, nothing inferred.
type proxiedFetcher struct {
	client     *http.Client
	proxy      *url.URL
	lastReq    time.Time
	attempts   int
	geoBlocked int // 403 responses verified as Yahoo's EEA/UK notice page
	http403    int // 403 responses whose body is NOT the EEA/UK notice page
	softPages  int // non-200 responses that returned a real Yahoo page body (e.g. a 404 search page)
	fetched    int // 200 responses
}

func newProxiedFetcher() (*proxiedFetcher, error) {
	u, err := resolveFetchProxy()
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy:             http.ProxyURL(u),
		DisableKeepAlives: true,
	}
	return &proxiedFetcher{
		client: &http.Client{Transport: tr, Timeout: 45 * time.Second},
		proxy:  u,
	}, nil
}

// pace enforces the standing human-pacing rule: at least ~3.5s, jittered,
// between requests to a host; never parallel.
func (f *proxiedFetcher) pace() {
	if f.lastReq.IsZero() {
		return
	}
	wait := time.Duration(3500+rand.Intn(3000)) * time.Millisecond
	if elapsed := time.Since(f.lastReq); elapsed < wait {
		time.Sleep(wait - elapsed)
	}
}

// isGeoBlockPage reports whether a response body is Yahoo JAPAN's EEA/UK
// notice page ("Yahoo! JAPAN is no longer available in the EEA and the
// United Kingdom", served with HTTP 403 to EEA/UK exit IPs). Verified
// against a captured page on 2026-11-05. This body check is the ONLY basis
// on which a response may be called a geo-block.
func isGeoBlockPage(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "欧州経済領域（EEA）およびイギリスからご利用のお客様へ") ||
		strings.Contains(s, "no longer available in the EEA and the United Kingdom")
}

// pageTitle extracts a short identifier for a response body: its <title>,
// trimmed and capped. Used so an unverified block can be reported by what
// the page actually says it is.
func pageTitle(body []byte) string {
	s := string(body)
	i := strings.Index(s, "<title>")
	if i < 0 {
		return "(no title)"
	}
	s = s[i+len("<title>"):]
	if j := strings.Index(s, "</title>"); j >= 0 {
		s = s[:j]
	}
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

var errGeoBlocked = fmt.Errorf("geo-blocked on every exit IP tried (each 403 verified as Yahoo's EEA/UK notice page)")

// fetch returns the page body and its HTTP status. Classification is exact:
//   - 200: success.
//   - 403 with the EEA/UK notice body: a verified geo-block - retried on a
//     fresh connection (new exit IP), up to 7 attempts.
//   - 403 with any other body: counted separately as an unverified 403 and
//     also retried (rotation may pass), but never reported as a geo-block;
//     if every attempt fails the error names the last page's title.
//   - 404 whose body is a real Yahoo page (carries __NEXT_DATA__): the body
//     is returned with status 404 and no error - Yahoo serves its
//     zero-result search pages this way, and the caller parses the payload
//     (which states its own result count) instead of failing the search.
//   - anything else: an error naming the status.
func (f *proxiedFetcher) fetch(rawURL string) ([]byte, int, error) {
	var lastErr error
	for attempt := 0; attempt < 7; attempt++ {
		f.pace()
		f.attempts++
		req, err := http.NewRequest("GET", rawURL, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
		req.Header.Set("Accept-Language", "ja,en-US;q=0.8,en;q=0.6")
		f.lastReq = time.Now()
		resp, err := f.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		switch {
		case resp.StatusCode == 200:
			f.fetched++
			return body, 200, nil
		case resp.StatusCode == 403 && isGeoBlockPage(body):
			f.geoBlocked++
			lastErr = errGeoBlocked
			continue
		case resp.StatusCode == 403:
			f.http403++
			lastErr = fmt.Errorf("HTTP 403 on every exit IP tried, page NOT identified as a geo-block (last page title: %q)", pageTitle(body))
			continue
		case resp.StatusCode == 404 && strings.Contains(string(body), "__NEXT_DATA__"):
			f.softPages++
			return body, 404, nil
		default:
			return nil, resp.StatusCode, fmt.Errorf("HTTP %d for %s", resp.StatusCode, rawURL)
		}
	}
	if lastErr != nil {
		return nil, 0, lastErr
	}
	return nil, 0, errGeoBlocked
}
