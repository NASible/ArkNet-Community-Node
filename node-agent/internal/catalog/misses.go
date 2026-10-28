package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"arknet/node-agent/internal/enrollment"
)

// MaxMisses matches Core's bound on one media-miss listing.
const MaxMisses = 100

// Miss is Core's aggregated report that this Node's home users fell back to
// Core for an active object. It carries no user or timing information.
type Miss struct {
	MediaID   string `json:"media_id"`
	MissCount int64  `json:"miss_count"`
}

// FetchMediaMisses performs the signed outbound read of this Node's misses.
func FetchMediaMisses(ctx context.Context, identity enrollment.Identity) ([]Miss, error) {
	if ctx.Err() != nil || identity.Status != "APPROVED" {
		return nil, ErrRequestFailed
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint := strings.TrimSuffix(identity.CoreURL, "/") + "/api/federation/v1/nodes/" + identity.NodeID + "/media-misses"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil || request.URL.RawQuery != "" || request.URL.User != nil || request.URL.Fragment != "" ||
		enrollment.SignRequest(request, identity.NodeID, identity.PrivateSeed, nil) != nil {
		return nil, ErrRequestFailed
	}
	client := boundedClient()
	response, err := client.Do(request)
	if err != nil {
		return nil, ErrRequestFailed
	}
	defer response.Body.Close()
	mediaType, _, typeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if response.StatusCode != http.StatusOK || typeErr != nil || mediaType != "application/json" || readErr != nil || len(data) > 64<<10 {
		return nil, ErrRequestFailed
	}
	var result struct {
		Misses []Miss `json:"misses"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Misses == nil || len(result.Misses) > MaxMisses {
		return nil, ErrRequestFailed
	}
	seen := make(map[string]bool, len(result.Misses))
	for _, miss := range result.Misses {
		if !validOpaqueID(miss.MediaID) || miss.MissCount < 1 || seen[miss.MediaID] {
			return nil, ErrRequestFailed
		}
		seen[miss.MediaID] = true
	}
	return result.Misses, nil
}
