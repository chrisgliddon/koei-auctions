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
// rotates the residential exit IP per request (a share of exit IPs are
// EEA/UK-geolocated and Yahoo geo-blocks those with a 403 - rotation plus
// bounded retries is what gets through).
type proxiedFetcher struct {
	client   *http.Client
	proxy    *url.URL
	lastReq  time.Time
	attempts int
	blocked  int
	fetched  int
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

var errBlocked = fmt.Errorf("blocked on every exit IP tried")

// fetch returns the page body. A 403 (Yahoo's geo-block page for EEA/UK exit
// IPs) is retried on a fresh connection - a new exit IP - up to 7 attempts.
func (f *proxiedFetcher) fetch(rawURL string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 7; attempt++ {
		f.pace()
		f.attempts++
		req, err := http.NewRequest("GET", rawURL, nil)
		if err != nil {
			return nil, err
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
		if resp.StatusCode == 200 {
			f.fetched++
			return body, nil
		}
		if resp.StatusCode == 403 {
			f.blocked++
			lastErr = errBlocked
			continue
		}
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, rawURL)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errBlocked
}
