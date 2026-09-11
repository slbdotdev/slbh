package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// ModelInfo is a model exposed by a configured provider. ID is the model
// string accepted by ForModel, so native models retain their provider prefix
// even when the provider's /models endpoint returns an unqualified ID.
type ModelInfo struct {
	ID            string
	Created       time.Time
	ContextWindow int
	Preferred     bool
}

// Catalog is one provider branch in the /models tree.
type Catalog struct {
	Name     string
	Endpoint string
	Models   []ModelInfo
	Err      string
}

type catalogSpec struct {
	name     string
	endpoint string
	key      string
}

// DiscoverCatalog discovers only providers for which a usable API key is
// present. OpenRouter is intentionally filtered to models created within the
// last year: its catalog is large enough that stale entries make the TUI
// impractical.
func DiscoverCatalog(ctx context.Context, endpointOverride string) ([]Catalog, error) {
	openRouterEndpoint := endpointOverride
	if openRouterEndpoint == "" {
		openRouterEndpoint = "https://openrouter.ai/api/v1/chat/completions"
	}
	catalogs := []Catalog{{
		Name:     LocalProviderName,
		Endpoint: localEndpoint(),
		Models: []ModelInfo{{
			ID:            LocalModelID,
			ContextWindow: LocalContextWindow,
			Preferred:     true,
		}},
	}}
	specs := []catalogSpec{
		{name: "deepseek", endpoint: "https://api.deepseek.com/chat/completions", key: os.Getenv("DEEPSEEK_API_KEY")},
		{name: "zai", endpoint: "https://api.z.ai/api/coding/paas/v4/chat/completions", key: os.Getenv("ZAI_API_KEY")},
		{name: "openrouter", endpoint: openRouterEndpoint, key: os.Getenv("OPENROUTER_API_KEY")},
	}

	var firstErr error
	for _, spec := range specs {
		if strings.TrimSpace(spec.key) == "" {
			continue
		}
		catalog, err := fetchCatalog(ctx, spec)
		if err != nil {
			catalog.Err = err.Error()
			if firstErr == nil {
				firstErr = err
			}
		}
		catalogs = append(catalogs, catalog)
	}
	markPreferred(catalogs)
	sort.Slice(catalogs, func(i, j int) bool { return catalogs[i].Name < catalogs[j].Name })
	return catalogs, firstErr
}

func fetchCatalog(ctx context.Context, spec catalogSpec) (Catalog, error) {
	catalog := Catalog{Name: spec.name, Endpoint: spec.endpoint}
	endpoint, err := modelsEndpoint(spec.endpoint)
	if err != nil {
		return catalog, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return catalog, err
	}
	request.Header.Set("Authorization", "Bearer "+spec.key)
	request.Header.Set("Accept", "application/json")
	client := http.DefaultClient
	response, err := client.Do(request)
	if err != nil {
		return catalog, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 8*1024))
		return catalog, fmt.Errorf("model catalog returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Data []modelMetadata `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 32*1024*1024)).Decode(&payload); err != nil {
		return catalog, fmt.Errorf("decode model catalog: %w", err)
	}
	cutoff := time.Now().AddDate(-1, 0, 0)
	for _, model := range payload.Data {
		if spec.name == "openrouter" && (model.Created <= 0 || time.Unix(model.Created, 0).Before(cutoff)) {
			continue
		}
		id := model.ID
		if spec.name != "openrouter" && !strings.Contains(id, "/") {
			id = spec.name + "/" + id
		}
		created := time.Time{}
		if model.Created > 0 {
			created = time.Unix(model.Created, 0)
		}
		window := model.ContextLength
		if window <= 0 {
			window = model.TopProvider.ContextLength
		}
		catalog.Models = append(catalog.Models, ModelInfo{ID: id, Created: created, ContextWindow: window})
	}
	sort.Slice(catalog.Models, func(i, j int) bool { return catalog.Models[i].ID < catalog.Models[j].ID })
	return catalog, nil
}

func markPreferred(catalogs []Catalog) {
	best := make(map[string]string)
	rank := func(name string) int {
		if name == "openrouter" {
			return 100
		}
		return 0
	}
	for _, catalog := range catalogs {
		for _, model := range catalog.Models {
			key := modelKey(model.ID)
			if current, ok := best[key]; !ok || rank(catalog.Name) < rank(current) {
				best[key] = catalog.Name
			}
		}
	}
	for i := range catalogs {
		for j := range catalogs[i].Models {
			catalogs[i].Models[j].Preferred = best[modelKey(catalogs[i].Models[j].ID)] == catalogs[i].Name
		}
	}
}

func modelKey(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if slash := strings.IndexByte(id, '/'); slash >= 0 {
		id = id[slash+1:]
	}
	return id
}
