package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type airtableClient struct {
	base      string
	table     string
	surrogate string
	http      *http.Client
}

func newAirtableClient(cfg *config) (*airtableClient, error) {
	s, err := fetchSurrogate()
	if err != nil {
		return nil, err
	}
	return &airtableClient{
		base: cfg.base, table: cfg.table, surrogate: s,
		http: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (a *airtableClient) do(method, path string, body any) (map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://api.airtable.com/v0/"+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.surrogate)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("airtable %s %s: HTTP %d: %s", method, path, resp.StatusCode, string(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("airtable response not JSON: %w", err)
	}
	time.Sleep(300 * time.Millisecond) // stay well under the 5 req/s base limit
	return out, nil
}

type atRecord struct {
	ID     string         `json:"id"`
	Fields map[string]any `json:"fields"`
}

func (a *airtableClient) listAll() ([]atRecord, error) {
	var all []atRecord
	offset := ""
	for {
		q := url.Values{"pageSize": {"100"}}
		if offset != "" {
			q.Set("offset", offset)
		}
		resp, err := a.do("GET", a.base+"/"+a.table+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		rawRecs, _ := json.Marshal(resp["records"])
		var recs []atRecord
		if err := json.Unmarshal(rawRecs, &recs); err != nil {
			return nil, err
		}
		all = append(all, recs...)
		off, _ := resp["offset"].(string)
		if off == "" {
			return all, nil
		}
		offset = off
	}
}

func (a *airtableClient) findByUUID(uuid string) (*atRecord, error) {
	q := url.Values{
		"filterByFormula": {fmt.Sprintf("{Listing UUID}='%s'", uuid)},
		"maxRecords":      {"1"},
	}
	resp, err := a.do("GET", a.base+"/"+a.table+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	rawRecs, _ := json.Marshal(resp["records"])
	var recs []atRecord
	if err := json.Unmarshal(rawRecs, &recs); err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return &recs[0], nil
}

func (a *airtableClient) create(fields map[string]any) (string, error) {
	resp, err := a.do("POST", a.base+"/"+a.table, map[string]any{"fields": fields})
	if err != nil {
		return "", err
	}
	id, _ := resp["id"].(string)
	if id == "" {
		return "", fmt.Errorf("create returned no record id")
	}
	return id, nil
}

func (a *airtableClient) get(recordID string) (*atRecord, error) {
	resp, err := a.do("GET", a.base+"/"+a.table+"/"+recordID, nil)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(resp)
	var rec atRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func strField(f map[string]any, key string) string {
	if v, ok := f[key].(string); ok {
		return v
	}
	return ""
}
