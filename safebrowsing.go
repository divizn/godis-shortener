package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const defaultSBEndpoint = "https://safebrowsing.googleapis.com/v4/threatMatches:find"

type safeBrowsing struct {
	endpoint string
	key      string
	client   *http.Client
}

func newSafeBrowsing(endpoint, key string) *safeBrowsing {
	if endpoint == "" {
		endpoint = defaultSBEndpoint
	}
	return &safeBrowsing{endpoint: endpoint, key: key, client: &http.Client{Timeout: 3 * time.Second}}
}

type sbRequest struct {
	Client     sbClient     `json:"client"`
	ThreatInfo sbThreatInfo `json:"threatInfo"`
}

type sbClient struct {
	ClientID      string `json:"clientId"`
	ClientVersion string `json:"clientVersion"`
}

type sbThreatInfo struct {
	ThreatTypes      []string  `json:"threatTypes"`
	PlatformTypes    []string  `json:"platformTypes"`
	ThreatEntryTypes []string  `json:"threatEntryTypes"`
	ThreatEntries    []sbEntry `json:"threatEntries"`
}

type sbEntry struct {
	URL string `json:"url"`
}

func (s *safeBrowsing) flagged(ctx context.Context, target string) (bool, error) {
	body, err := json.Marshal(sbRequest{
		Client: sbClient{ClientID: "godis-shortener", ClientVersion: "1"},
		ThreatInfo: sbThreatInfo{
			ThreatTypes:      []string{"MALWARE", "SOCIAL_ENGINEERING", "UNWANTED_SOFTWARE", "POTENTIALLY_HARMFUL_APPLICATION"},
			PlatformTypes:    []string{"ANY_PLATFORM"},
			ThreatEntryTypes: []string{"URL"},
			ThreatEntries:    []sbEntry{{URL: target}},
		},
	})
	if err != nil {
		return false, fmt.Errorf("encode safe browsing request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint+"?key="+url.QueryEscape(s.key), bytes.NewReader(body))
	if err != nil {
		return false, errors.New("build safe browsing request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		// the url error carries the request url, which holds the api key
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return false, fmt.Errorf("safe browsing lookup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("safe browsing lookup: status %d", resp.StatusCode)
	}
	var out struct {
		Matches []json.RawMessage `json:"matches"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return false, fmt.Errorf("decode safe browsing response: %w", err)
	}
	return len(out.Matches) > 0, nil
}
