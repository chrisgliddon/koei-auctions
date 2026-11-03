package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// fetchSurrogate asks the runtime authd (unix socket) for the surrogate
// credential entry for the stored custom.airtable connection. The returned
// value is an hsurr: placeholder the egress proxy exchanges for the real
// token; the real API key never reaches this process.
func fetchSurrogate() (string, error) {
	sock := os.Getenv("JARVIS_AUTHD_SOCK")
	if sock == "" {
		sock = "/run/hatch/auth/authd.sock"
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]string{"name": "custom.airtable"})
	resp, err := client.Post("http://authd.local/v1/credentials/surrogate", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("authd surrogate request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("authd returned HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var payload struct {
		Credentials []struct {
			Name      string          `json:"name"`
			Surrogate string          `json:"surrogate"`
			Placement json.RawMessage `json:"placement"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("authd response not JSON: %w", err)
	}
	for _, e := range payload.Credentials {
		if e.Name != "access_token" {
			continue
		}
		var placement string
		if err := json.Unmarshal(e.Placement, &placement); err != nil || placement != "bearer_header" {
			return "", fmt.Errorf("unexpected credential placement: %s", string(e.Placement))
		}
		if e.Surrogate == "" {
			return "", fmt.Errorf("empty surrogate from authd")
		}
		return e.Surrogate, nil
	}
	return "", fmt.Errorf("no access_token surrogate for custom.airtable")
}
