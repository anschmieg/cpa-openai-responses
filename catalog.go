package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const maxCatalogBytes = 4 << 20

var catalogClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

type modelCatalog struct {
	refreshMu sync.Mutex
	mu        sync.RWMutex
	namespace [32]byte
	ids       []string
}

func (c *modelCatalog) refresh(cfg config) ([]string, error) {
	if cfg.apiKey == "" {
		return nil, nil
	}
	if len(cfg.Models) != 0 {
		return cfg.Models, nil
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	ids, err := fetchModels(catalogClient, cfg)
	c.mu.Lock()
	c.namespace, c.ids = cfg.namespace, ids
	c.mu.Unlock()
	return ids, err
}

func (c *modelCatalog) contains(cfg config, model string) bool {
	if len(cfg.Models) != 0 {
		return slices.Contains(cfg.Models, model)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.namespace == cfg.namespace && slices.Contains(c.ids, model)
}

func fetchModels(client *http.Client, cfg config) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, cfg.BaseURL+"/models", nil)
	if err != nil {
		return nil, errors.New("invalid upstream model catalog URL")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("upstream model discovery request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("upstream model discovery returned a non-200 status")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	var catalog struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err != nil || len(raw) > maxCatalogBytes || json.Unmarshal(raw, &catalog) != nil || catalog.Data == nil {
		return nil, errors.New("invalid upstream model catalog")
	}
	ids := make([]string, 0, len(catalog.Data))
	for _, row := range catalog.Data {
		if responsesModel(row.ID) {
			ids = append(ids, row.ID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// OpenAI's model list does not describe endpoint compatibility. Default to
// known text-generation families and exclude variants with dedicated endpoints.
func responsesModel(id string) bool {
	if !modelID.MatchString(id) || strings.Contains(id, ":") {
		return false
	}
	base := id
	for _, prefix := range []string{"gpt-4.1", "gpt-4o", "gpt-5", "gpt-6", "o1", "o3", "o4-mini"} {
		if base != prefix && !strings.HasPrefix(base, prefix+"-") && !strings.HasPrefix(base, prefix+".") {
			continue
		}
		for _, part := range strings.Split(base, "-") {
			switch part {
			case "audio", "realtime", "transcribe", "tts", "image", "search", "codex", "deep", "research":
				return false
			}
		}
		return true
	}
	return false
}
