package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/munlucky/codex-account-pool/internal/antigravityauth"
)

type quotaState int

const (
	quotaUnknown quotaState = iota
	quotaUsable
	quotaExhausted
)

func (c *Client) probeModelQuota(ctx context.Context, credentials antigravityauth.Credentials, wireModel string) quotaState {
	body, _ := json.Marshal(map[string]any{"project": credentials.ProjectID})
	endpoint := strings.TrimRight(c.baseURL(), "/") + "/v1internal:fetchAvailableModels"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return quotaUnknown
	}
	c.setHeaders(req, credentials.AccessToken)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return quotaUnknown
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return quotaUnknown
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return quotaUnknown
	}
	return quotaStateForModel(payload, wireModel)
}

func quotaStateForModel(payload map[string]any, wireModel string) quotaState {
	models, _ := payload["models"].(map[string]any)
	if len(models) == 0 {
		return quotaUnknown
	}
	modelInfo, _ := models[wireModel].(map[string]any)
	if modelInfo == nil {
		return quotaUnknown
	}
	entries := quotaEntries(modelInfo)
	if len(entries) == 0 {
		return quotaUnknown
	}
	known := 0
	for _, entry := range entries {
		remaining, ok := quotaRemaining(entry)
		if !ok {
			continue
		}
		known++
		if remaining <= 0 {
			return quotaExhausted
		}
	}
	if known == 0 {
		return quotaUnknown
	}
	return quotaUsable
}

func quotaEntries(modelInfo map[string]any) []map[string]any {
	var out []map[string]any
	appendValue := func(value any) {
		switch v := value.(type) {
		case map[string]any:
			out = append(out, v)
		case []any:
			for _, raw := range v {
				if row, ok := raw.(map[string]any); ok {
					out = append(out, row)
				}
			}
		}
	}
	appendValue(modelInfo["quotaInfo"])
	appendValue(modelInfo["quotaInfos"])
	if byTier, ok := modelInfo["quotaInfoByTier"].(map[string]any); ok {
		for _, value := range byTier {
			appendValue(value)
		}
	}
	return out
}

func quotaRemaining(entry map[string]any) (float64, bool) {
	target := entry
	if nested, ok := entry["remaining"].(map[string]any); ok {
		target = nested
	}
	if value, ok := numericFloat(target["remainingFraction"]); ok {
		return value, true
	}
	if value, ok := numericFloat(target["remainingPercentage"]); ok {
		return value, true
	}
	return 0, false
}

func numericFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	default:
		return 0, false
	}
}

func quotaProbeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, 5*time.Second)
}
