package provider

import (
	"context"
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

// catalogSpec is one catalog fetch: where it lives, what speaks to it, and
// which credential it needs.
//
// Since phase 2 these are derived from the routing policy rather than
// hardcoded here. That is not tidiness: the plan route moved to a wire whose
// catalog URL cannot be derived from its inference URL by suffix-stripping and
// whose document `modelMetadata` cannot decode, so a hardcoded list would have
// gone on fetching the wrong path with the wrong parser. The policy already
// carries the endpoint, the catalog endpoint and the wire; this reads them.
type catalogSpec struct {
	name            string
	endpoint        string
	catalogEndpoint string
	wire            string
	key             string
}

// DiscoverCatalog discovers only providers for which a usable API key is
// present. OpenRouter is intentionally filtered to models created within the
// last year: its catalog is large enough that stale entries make the TUI
// impractical.
//
// Routes are grouped by provider family, because several route keys share one
// catalog — `zai/glm-5.3-flash` and `zai/glm-5.3` are two routes and one
// endpoint — and fetching it twice would be two round trips for one answer.
func DiscoverCatalog(ctx context.Context, policy Policy) ([]Catalog, error) {
	catalogs := []Catalog{localCatalog(policy)}

	var firstErr error
	for _, spec := range catalogSpecsFor(policy) {
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

// localCatalog is the one branch that is listed rather than fetched. Ollama's
// OpenAI-compatible catalog publishes no context length and the local route
// carries no credential, so a fetch would return ids with no window behind
// them; the policy already states both, and it is the same document the
// request will be routed by.
//
// It is derived from the policy rather than compiled in because the desktop
// serves several quants at once — one per context rung — and which ones they
// are is a deployment fact that changes by converge. A build that named them
// would list a model the desktop had stopped serving, which is exactly what it
// did between 2026-09-15 and 2026-09-17. The compiled default survives as the
// answer for a policy that names no local route at all, so a stripped-down
// policy still reaches the desktop.
func localCatalog(policy Policy) Catalog {
	catalog := Catalog{Name: LocalProviderName, Endpoint: localEndpoint()}
	for _, key := range sortedKeys(policy.Routes) {
		if native, isNative := nativeRouteFor(key); !isNative || native.flavor != LocalProviderName {
			continue
		}
		window := policy.Routes[key].ContextWindow
		if window <= 0 {
			window = LocalContextWindow
		}
		catalog.Models = append(catalog.Models, ModelInfo{ID: key, ContextWindow: window, Preferred: true})
	}
	if len(catalog.Models) == 0 {
		catalog.Models = []ModelInfo{{ID: LocalModelID, ContextWindow: LocalContextWindow, Preferred: true}}
	}
	return catalog
}

// catalogSpecsFor derives one spec per provider family the policy describes
// and a credential is present for. A route whose wire this build cannot speak
// is skipped rather than fetched: its catalog document would not decode, and
// listing models for a route that refuses to run is a menu entry that can only
// disappoint.
func catalogSpecsFor(policy Policy) []catalogSpec {
	byFamily := map[string]catalogSpec{}
	for _, key := range sortedKeys(policy.Routes) {
		route := policy.Routes[key]
		if !WireSupported(route.Wire) {
			continue
		}
		native, isNative := nativeRouteFor(key)
		family, keyEnv := "openrouter", "OPENROUTER_API_KEY"
		if isNative {
			if native.flavor == LocalProviderName {
				continue // already listed, and it needs no credential
			}
			family, keyEnv = native.flavor, native.keyEnv
		}
		if _, seen := byFamily[family]; seen {
			continue
		}
		credential := strings.TrimSpace(os.Getenv(keyEnv))
		if credential == "" {
			continue
		}
		catalogEndpoint := strings.TrimSpace(route.CatalogEndpoint)
		if catalogEndpoint == "" {
			derived, err := modelsEndpoint(route.Endpoint)
			if err != nil {
				continue
			}
			catalogEndpoint = derived
		}
		byFamily[family] = catalogSpec{
			name:            family,
			endpoint:        route.Endpoint,
			catalogEndpoint: catalogEndpoint,
			wire:            route.Wire,
			key:             credential,
		}
	}
	specs := make([]catalogSpec, 0, len(byFamily))
	for _, family := range []string{"deepseek", "zai", "openrouter"} {
		if spec, ok := byFamily[family]; ok {
			specs = append(specs, spec)
			delete(byFamily, family)
		}
	}
	for _, name := range sortedSpecNames(byFamily) {
		specs = append(specs, byFamily[name])
	}
	return specs
}

func sortedSpecNames(specs map[string]catalogSpec) []string {
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func fetchCatalog(ctx context.Context, spec catalogSpec) (Catalog, error) {
	catalog := Catalog{Name: spec.name, Endpoint: spec.endpoint}
	strategy, ok := wireFor(spec.wire)
	if !ok {
		return catalog, fmt.Errorf("no implementation for wire %q", spec.wire)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.catalogEndpoint, nil)
	if err != nil {
		return catalog, err
	}
	// The wire owns auth headers here too: this endpoint's catalog wants the
	// same credential shape its inference path does.
	strategy.applyHeaders(request.Header, spec.key)
	request.Header.Set("Accept", "application/json")
	client := http.DefaultClient
	response, err := client.Do(request)
	if err != nil {
		return catalog, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 8*1024))
		return catalog, strategy.classifyError(response.StatusCode, body)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 32*1024*1024))
	if err != nil {
		return catalog, err
	}
	models, err := strategy.decodeCatalog(body)
	if err != nil {
		return catalog, err
	}
	cutoff := time.Now().AddDate(-1, 0, 0)
	for _, model := range models {
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
