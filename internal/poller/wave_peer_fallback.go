package poller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// Candidate endpoints to probe for Wave/MLO peer lists.
//
// NOTE: This is a best-effort fallback to help when certain firmware families
// omit wireless.peers from /api/v1.0/statistics.
//
// Guarded behind Poller.Config.WavePeerFallback and/or Ultra Debug probing.
var wavePeerCandidatePaths = []string{
	"/api/v1.0/wireless/peers",
	"/api/v1.0/statistics/wireless/peers",
	"/api/v1.0/peers",
}

// probeWavePeerEndpoints performs GETs to known candidate endpoints. The intent is
// to capture responses in Ultra Debug logs without changing runtime behavior.
func (p *Poller) probeWavePeerEndpoints(client *http.Client, baseURL, token string) {
	for _, path := range wavePeerCandidatePaths {
		url := baseURL + path
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("X-Auth-Token", token)

		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// fetchWavePeersFallback attempts to retrieve peers from alternate endpoints and returns
// the first non-empty peer list.
func (p *Poller) fetchWavePeersFallback(client *http.Client, baseURL, token string) ([]interface{}, string, error) {
	var lastErr error
	for _, path := range wavePeerCandidatePaths {
		url := baseURL + path
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("X-Auth-Token", token)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode != 200 {
			continue
		}

		peers, err := parseWavePeerPayload(body)
		if err != nil {
			lastErr = err
			continue
		}
		if len(peers) > 0 {
			return peers, path, nil
		}
	}
	return nil, "", lastErr
}

func parseWavePeerPayload(body []byte) ([]interface{}, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}

	var v interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}

	switch t := v.(type) {
	case []interface{}:
		return t, nil
	case map[string]interface{}:
		if peers, ok := t["peers"].([]interface{}); ok {
			return peers, nil
		}
		if wireless, ok := t["wireless"].(map[string]interface{}); ok {
			if peers, ok := wireless["peers"].([]interface{}); ok {
				return peers, nil
			}
		}
	}

	return nil, nil
}
