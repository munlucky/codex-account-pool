package antigravity

import (
	"sort"
	"strings"

	"github.com/munlucky/codex-account-pool/internal/openaiapi"
)

var fallbackModels = []openaiapi.Model{
	{ID: "gemini-3.8-flash", Object: "model", OwnedBy: "google-antigravity"},
	{ID: "gemini-3.7-flash", Object: "model", OwnedBy: "google-antigravity"},
	{ID: "gemini-3.1-pro", Object: "model", OwnedBy: "google-antigravity"},
	{ID: "gemini-3.1-flash-image", Object: "model", OwnedBy: "google-antigravity"},
	{ID: "claude-sonnet-4-6", Object: "model", OwnedBy: "google-antigravity"},
	{ID: "claude-opus-4-6-thinking", Object: "model", OwnedBy: "google-antigravity"},
	{ID: "gpt-oss-120b-medium", Object: "model", OwnedBy: "google-antigravity"},
}

func staticModels() []openaiapi.Model {
	return append([]openaiapi.Model(nil), fallbackModels...)
}

func parseAvailableModels(payload map[string]any) []openaiapi.Model {
	modelsMap, ok := payload["models"].(map[string]any)
	if !ok || len(modelsMap) == 0 {
		return nil
	}
	ids := make([]string, 0, len(modelsMap))
	seenWire := map[string]bool{}
	if sorts, ok := payload["agentModelSorts"].([]any); ok {
		for _, rawSort := range sorts {
			sortRow, _ := rawSort.(map[string]any)
			groups, _ := sortRow["groups"].([]any)
			for _, rawGroup := range groups {
				group, _ := rawGroup.(map[string]any)
				modelIDs, _ := group["modelIds"].([]any)
				for _, rawID := range modelIDs {
					id, _ := rawID.(string)
					if id != "" && modelsMap[id] != nil && !seenWire[id] {
						seenWire[id] = true
						ids = append(ids, id)
					}
				}
			}
		}
	}
	if images, ok := payload["imageGenerationModelIds"].([]any); ok {
		for _, rawID := range images {
			id, _ := rawID.(string)
			if id == "gemini-3.1-flash-image" && modelsMap[id] != nil && !seenWire[id] {
				seenWire[id] = true
				ids = append(ids, id)
			}
		}
	}
	if tiered, ok := payload["tieredModelIds"].(map[string]any); ok {
		if flash, ok := tiered["flash"].([]any); ok {
			for _, rawID := range flash {
				id, _ := rawID.(string)
				if id != "" && modelsMap[id] != nil && !seenWire[id] {
					seenWire[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	available := map[string]bool{}
	for _, id := range ids {
		available[id] = true
	}
	seen := map[string]bool{}
	var result []openaiapi.Model
	for _, wireID := range ids {
		id := pickerModelID(wireID, available)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, openaiapi.Model{ID: id, Object: "model", OwnedBy: "google-antigravity"})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func pickerModelID(wireID string, available map[string]bool) string {
	if strings.HasSuffix(wireID, "-tiered") {
		return strings.TrimSuffix(wireID, "-tiered")
	}
	if strings.HasPrefix(wireID, "gemini-3.8-flash-") {
		if available["gemini-3.8-flash-low"] && available["gemini-3.8-flash-medium"] && available["gemini-3.8-flash-high"] {
			return "gemini-3.8-flash"
		}
	}
	if wireID == "gemini-3.1-pro-low" || wireID == "gemini-pro-agent" {
		if available["gemini-3.1-pro-low"] && available["gemini-pro-agent"] {
			return "gemini-3.1-pro"
		}
	}
	return wireID
}
