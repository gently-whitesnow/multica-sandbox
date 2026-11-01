package inference

import (
	"encoding/json"
	"maps"
	"slices"
)

type cliproxyModelList struct {
	Data []struct {
		ID    string `json:"id"`
		Owner string `json:"owned_by"`
	} `json:"data"`
}

type cliproxyMetadataList struct {
	Models []cliproxyMetadata `json:"models"`
}

type cliproxyMetadata struct {
	Slug       string `json:"slug"`
	Label      string `json:"display_name"`
	Context    int    `json:"context_window"`
	Output     int    `json:"max_tokens"`
	Supported  bool   `json:"supported_in_api"`
	Visibility string `json:"visibility"`
	Default    string `json:"default_reasoning_level"`
	Levels     []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
}

// cliproxyModelsCatalog joins available IDs with the rich Codex-client
// metadata. Only typed model fields survive; provider instructions stay outside.
func cliproxyModelsCatalog(modelsData, metadataData []byte, defaultModel string) (Catalog, error) {
	var available cliproxyModelList
	var rich cliproxyMetadataList
	if json.Unmarshal(modelsData, &available) != nil || available.Data == nil || json.Unmarshal(metadataData, &rich) != nil || rich.Models == nil {
		return Catalog{}, ErrDenied
	}
	metadata := map[string]cliproxyMetadata{}
	for _, entry := range rich.Models {
		if _, exists := metadata[entry.Slug]; !exists {
			metadata[entry.Slug] = entry
		}
	}
	models := map[string]Model{}
	for _, entry := range available.Data {
		if _, exists := models[entry.ID]; exists || !validModel(entry.ID) {
			continue
		}
		api, ok := wireAPI(entry.Owner, entry.ID, "chat")
		if !ok {
			continue
		}
		model := Model{API: api}
		if detail, exists := metadata[entry.ID]; exists {
			if !detail.Supported || detail.Visibility == "hide" {
				continue
			}
			model = detail.model(api)
		}
		if api == APIChat {
			model.API = ""
		}
		models[entry.ID] = model
	}
	names := slices.Sorted(maps.Keys(models))
	for _, name := range names[min(len(names), gatewayModels):] {
		delete(models, name)
	}
	catalog := Catalog{Models: models}
	if _, exists := models[defaultModel]; exists {
		catalog.DefaultModel = defaultModel
	}
	if !validCatalogSize(catalog, gatewayModels) {
		return Catalog{}, ErrDenied
	}
	return catalog, nil
}

func (d cliproxyMetadata) model(api string) Model {
	label := d.Label
	if label != "" && label != d.Slug {
		label += " (" + d.Slug + ")"
	}
	model := Model{API: api, Label: label, Context: d.Context, Output: d.Output}
	if len(d.Levels) > 0 {
		details := modelDetails{DefaultLevel: d.Default}
		for _, level := range d.Levels {
			details.Levels = append(details.Levels, level.Effort)
		}
		model.Thinking = details.thinking(api)
	}
	return model
}
