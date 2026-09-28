package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type localModelsResp struct {
	Data []struct {
		ID string `json:"id"`
		// Context window, when the server reports it. Field names vary:
		// vLLM sends max_model_len; OpenRouter-style gateways and some
		// local servers send context_length / max_context_length /
		// context_window. Training-time values (llama.cpp n_ctx_train) are
		// deliberately ignored: the runtime window can be much smaller.
		MaxModelLen      int `json:"max_model_len"`
		ContextLength    int `json:"context_length"`
		MaxContextLength int `json:"max_context_length"`
		ContextWindow    int `json:"context_window"`
	} `json:"data"`
}

// ProbeLocalServer contacts baseURL/v1/models and returns the available models.
// apiKey is optional: servers started with authentication (llama-server
// --api-key, vLLM, remote LM Studio, …) get it as a Bearer token; pass "" for
// open servers. Returns an error if the server is unreachable or the response
// is invalid.
func ProbeLocalServer(ctx context.Context, baseURL, apiKey string) ([]Model, error) {
	baseURL = LocalProviderID(baseURL)

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("server not reachable at %s: %w", baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if apiKey == "" {
			return nil, fmt.Errorf("server requires an API key (HTTP %d)", resp.StatusCode)
		}
		return nil, fmt.Errorf("server rejected the API key (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var lmResp localModelsResp
	if err := json.Unmarshal(body, &lmResp); err != nil {
		return nil, fmt.Errorf("invalid /v1/models response: %w", err)
	}

	provName := LocalProviderName(baseURL)
	out := make([]Model, 0, len(lmResp.Data))
	for _, m := range lmResp.Data {
		if m.ID == "" {
			continue
		}
		ctxWindow := m.MaxModelLen
		for _, v := range []int{m.ContextLength, m.MaxContextLength, m.ContextWindow} {
			if ctxWindow <= 0 {
				ctxWindow = v
			}
		}
		out = append(out, Model{
			Provider:     baseURL,
			ProviderName: provName,
			Name:         m.ID,
			DisplayName:  m.ID,
			Context:      max(ctxWindow, 0),
			Local:        true,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("server is running but returned no models")
	}
	return out, nil
}

// LocalProviderID returns the provider id the models of a local endpoint
// get: the endpoint URL without a trailing slash, with http:// added when it
// has no scheme.
func LocalProviderID(endpoint string) string {
	id := strings.TrimRight(endpoint, "/")
	if !strings.HasPrefix(id, "http") {
		id = "http://" + id
	}
	return id
}

// LocalProviderName derives a human-readable name from a local server URL.
func LocalProviderName(baseURL string) string {
	s := strings.TrimPrefix(baseURL, "http://")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimRight(s, "/")
	switch {
	case strings.HasSuffix(s, ":1234"):
		return "LM Studio"
	case strings.HasSuffix(s, ":11434"):
		return "Ollama"
	case strings.HasSuffix(s, ":8080"):
		return "llama.cpp"
	case strings.HasSuffix(s, ":8888"):
		return "Unsloth"
	case strings.HasSuffix(s, ":8000"):
		return "vLLM"
	default:
		return "Local endpoint"
	}
}
