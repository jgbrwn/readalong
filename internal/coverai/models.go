package coverai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultReflectionURL = "https://reflection.int.exe.xyz/integrations"
	maxDiscoveryBody     = 8 << 20
	maxDiscoveredModels  = 2048
)

type APIStyle string

const (
	APIResponses APIStyle = "responses"
	APIChat      APIStyle = "chat_completions"
)

type Settings struct {
	Enabled              bool     `json:"enabled"`
	CatalogLookupEnabled bool     `json:"catalog_lookup_enabled"`
	UseBookDescription   bool     `json:"use_book_description"`
	ModelID              string   `json:"model_id,omitempty"`
	APIStyle             APIStyle `json:"api_style,omitempty"`
}

// Model is the deliberately small, browser-safe subset of Reflection's model
// inventory used for cover design. Vision means image input, not image output.
type Model struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Provider         string   `json:"provider,omitempty"`
	Description      string   `json:"description,omitempty"`
	APIStyle         APIStyle `json:"api_style"`
	Vision           bool     `json:"vision"`
	ImageOutput      bool     `json:"image_output"`
	InputPerMillion  *float64 `json:"input_per_million,omitempty"`
	OutputPerMillion *float64 `json:"output_per_million,omitempty"`
}

type Registry struct {
	ReflectionURL string
	HTTP          *http.Client
}

type CheckResult struct {
	Healthy   bool     `json:"healthy"`
	Message   string   `json:"message"`
	APIStyle  APIStyle `json:"api_style"`
	LatencyMS int64    `json:"latency_ms"`
}

type integrationList struct {
	Integrations []integration `json:"integrations"`
}

type integration struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Comment string `json:"comment"`
	Help    string `json:"help"`
}

type modelList struct {
	Data []json.RawMessage `json:"data"`
}

var integrationURLPattern = regexp.MustCompile(`https?://[A-Za-z0-9.-]+(?::[0-9]{1,5})?(?:/[A-Za-z0-9._~!$&'()*+,;=:@%-]*)?`)

func NewRegistry() *Registry {
	return &Registry{
		ReflectionURL: defaultReflectionURL,
		HTTP:          &http.Client{Timeout: 12 * time.Second},
	}
}

// Discover resolves the attached LLM integration through Reflection, fetches
// its OpenAI-compatible model list, and returns only models suitable for image
// input or explicit image output. It never invokes a model.
func (r *Registry) Discover(ctx context.Context) ([]Model, error) {
	modelsURL, err := r.resolveModelsURL(ctx)
	if err != nil {
		return nil, err
	}
	var catalog modelList
	if err := r.getJSON(ctx, modelsURL.String(), &catalog, modelsURL.Hostname()); err != nil {
		return nil, fmt.Errorf("managed LLM model catalog is unavailable")
	}
	if len(catalog.Data) > maxDiscoveredModels {
		return nil, fmt.Errorf("managed LLM model catalog is too large")
	}
	models := make([]Model, 0)
	seen := make(map[string]struct{})
	for _, raw := range catalog.Data {
		model, ok := parseModel(raw)
		if !ok || (!model.Vision && !model.ImageOutput) {
			continue
		}
		if _, duplicate := seen[model.ID]; duplicate {
			continue
		}
		seen[model.ID] = struct{}{}
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Provider != models[j].Provider {
			return strings.ToLower(models[i].Provider) < strings.ToLower(models[j].Provider)
		}
		return strings.ToLower(models[i].Name) < strings.ToLower(models[j].Name)
	})
	return models, nil
}

func (r *Registry) resolveModelsURL(ctx context.Context) (*url.URL, error) {
	reflectionURL := strings.TrimSpace(r.ReflectionURL)
	if reflectionURL == "" {
		reflectionURL = defaultReflectionURL
	}
	reflection, err := url.Parse(reflectionURL)
	if err != nil || !validDiscoveryURL(reflection) || reflection.RawQuery != "" || reflection.Fragment != "" {
		return nil, fmt.Errorf("Reflection discovery endpoint is invalid")
	}

	var inventory integrationList
	if err := r.getJSON(ctx, reflection.String(), &inventory, reflection.Hostname()); err != nil {
		return nil, fmt.Errorf("Reflection integration discovery is unavailable")
	}
	for _, item := range inventory.Integrations {
		if !strings.EqualFold(strings.TrimSpace(item.Type), "llm") {
			continue
		}
		modelsURL, err := modelDiscoveryURL(item)
		if err == nil && validDiscoveryURL(modelsURL) && modelsURL.RawQuery == "" && modelsURL.Fragment == "" {
			return modelsURL, nil
		}
	}
	return nil, fmt.Errorf("no managed LLM integration is attached to this VM")
}

// CheckModel sends one short, non-streaming completion to the selected model.
// It is intentionally separate from discovery because providers may charge
// even a tiny inference request.
func (r *Registry) CheckModel(ctx context.Context, modelID string, style APIStyle) (CheckResult, error) {
	if modelID == "" || len(modelID) > 255 || hasControl(modelID) {
		return CheckResult{}, fmt.Errorf("select a valid model")
	}
	if style != APIResponses && style != APIChat {
		return CheckResult{}, fmt.Errorf("select a supported API style")
	}
	modelsURL, err := r.resolveModelsURL(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	checkURL := *modelsURL
	if style == APIResponses {
		checkURL.Path = "/v1/responses"
	} else {
		checkURL.Path = "/v1/chat/completions"
	}
	var payload any
	if style == APIResponses {
		payload = map[string]any{
			"model": modelID, "input": "Reply with OK.", "max_output_tokens": 8,
		}
	} else {
		payload = map[string]any{
			"model":      modelID,
			"messages":   []map[string]string{{"role": "user", "content": "Reply with OK."}},
			"max_tokens": 8, "stream": false,
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return CheckResult{}, fmt.Errorf("could not prepare model check")
	}
	started := time.Now()
	responseBody, status, err := r.do(ctx, http.MethodPost, checkURL.String(), body, modelsURL.Hostname())
	result := CheckResult{APIStyle: style, LatencyMS: time.Since(started).Milliseconds()}
	if err != nil {
		result.Message = "The managed LLM service could not be reached."
		return result, nil
	}
	if status >= 200 && status < 300 {
		if !hasModelText(responseBody, style) {
			result.Message = "The model endpoint responded, but returned no text. Check the selected API style."
			return result, nil
		}
		result.Healthy = true
		result.Message = "Model is responding; the small health check succeeded."
		return result, nil
	}
	result.Message = modelCheckFailure(status, responseBody)
	return result, nil
}

func modelDiscoveryURL(item integration) (*url.URL, error) {
	for _, description := range []string{item.Help, item.Comment} {
		for _, rawURL := range integrationURLPattern.FindAllString(description, -1) {
			u, err := url.Parse(rawURL)
			if err != nil {
				continue
			}
			if u.Path == "/v1/models" {
				return u, nil
			}
			if u.Path == "/v1" || u.Path == "" || u.Path == "/" {
				u.Path = "/v1/models"
				return u, nil
			}
		}
	}
	name := strings.ToLower(strings.TrimSpace(item.Name))
	if name == "" || len(name) > 63 {
		return nil, fmt.Errorf("managed LLM integration has no safe model endpoint")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return nil, fmt.Errorf("managed LLM integration has no safe model endpoint")
	}
	return &url.URL{
		Scheme: "https",
		Host:   name + ".int.exe.xyz",
		Path:   "/v1/models",
	}, nil
}

func (r *Registry) getJSON(ctx context.Context, rawURL string, dst any, expectedHost string) error {
	body, status, err := r.do(ctx, http.MethodGet, rawURL, nil, expectedHost)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("discovery request returned HTTP %d", status)
	}
	return json.Unmarshal(body, dst)
}

func (r *Registry) do(ctx context.Context, method, rawURL string, body []byte, expectedHost string) ([]byte, int, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || !validDiscoveryURL(parsed) || !strings.EqualFold(parsed.Hostname(), expectedHost) {
		return nil, 0, fmt.Errorf("discovery request URL is invalid")
	}
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	safeClient := *client
	expectedHost = strings.ToLower(expectedHost)
	safeClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 2 || !validDiscoveryURL(req.URL) ||
			!strings.EqualFold(req.URL.Hostname(), expectedHost) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	var requestBody io.Reader
	if len(body) > 0 {
		if len(body) > maxDiscoveryBody {
			return nil, 0, fmt.Errorf("request body is too large")
		}
		requestBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, requestBody)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Readalong/1.0 model discovery")
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := safeClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBody+1))
	if err != nil || len(responseBody) > maxDiscoveryBody {
		return nil, resp.StatusCode, fmt.Errorf("discovery response could not be read")
	}
	return responseBody, resp.StatusCode, nil
}

func parseModel(raw json.RawMessage) (Model, bool) {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return Model{}, false
	}
	id := stringValue(value["id"])
	if id == "" || len(id) > 255 || hasControl(id) {
		return Model{}, false
	}
	metadata := mapValue(value["metadata"])
	capabilities := mapValue(metadata["capabilities"])
	if capabilities == nil {
		capabilities = mapValue(value["capabilities"])
	}
	name := firstString(
		metadata["display_name"], value["name"], value["canonical_slug"], id,
	)
	provider := firstString(metadata["provider"], value["owned_by"])
	description := firstString(metadata["description"], value["description"])
	if len(name) > 255 || hasControl(name) {
		name = id
	}
	if len(provider) > 100 || hasControl(provider) {
		provider = ""
	}
	if len(description) > 500 || hasControl(description) {
		description = ""
	}
	model := Model{
		ID:          id,
		Name:        name,
		Provider:    provider,
		Description: description,
		APIStyle:    inferAPIStyle(value, metadata, provider, id),
		Vision:      boolValue(capabilities["vision"]),
		ImageOutput: hasImageOutput(value, metadata),
	}
	if pricing := mapValue(metadata["pricing"]); pricing != nil {
		model.InputPerMillion = optionalFloat(pricing["input_per_million"])
		model.OutputPerMillion = optionalFloat(pricing["output_per_million"])
	}
	if deprecated, _ := metadata["deprecated"].(bool); deprecated {
		return Model{}, false
	}
	return model, model.Vision || model.ImageOutput
}

func inferAPIStyle(model, metadata map[string]any, provider, id string) APIStyle {
	for _, key := range []string{"api_style", "api_type", "api", "endpoint", "endpoints", "supported_endpoints"} {
		for _, root := range []map[string]any{model, metadata} {
			raw := strings.ToLower(fmt.Sprint(root[key]))
			chat := strings.Contains(raw, "chat/completions") || strings.Contains(raw, "chat_completions")
			responses := strings.Contains(raw, "/responses") || raw == "responses"
			if chat && !responses {
				return APIChat
			}
			if responses && !chat {
				return APIResponses
			}
		}
	}
	owner := strings.ToLower(stringValue(model["owned_by"]))
	if owner == "chatgpt" || strings.HasPrefix(strings.ToLower(id), "openai/") ||
		strings.Contains(strings.ToLower(provider), "openai") {
		return APIResponses
	}
	return APIChat
}

func validDiscoveryURL(u *url.URL) bool {
	if u == nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "https" && strings.HasSuffix(host, ".int.exe.xyz") {
		return u.Port() == "" || u.Port() == "443"
	}
	if u.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hasModelText(body []byte, style APIStyle) bool {
	var response map[string]any
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	if style == APIResponses {
		output, _ := response["output"].([]any)
		for _, item := range output {
			message := mapValue(item)
			content, _ := message["content"].([]any)
			for _, part := range content {
				if strings.TrimSpace(stringValue(mapValue(part)["text"])) != "" {
					return true
				}
			}
		}
		return false
	}
	choices, _ := response["choices"].([]any)
	for _, choice := range choices {
		message := mapValue(mapValue(choice)["message"])
		if strings.TrimSpace(stringValue(message["content"])) != "" {
			return true
		}
	}
	return false
}

func modelCheckFailure(status int, body []byte) string {
	text := strings.ToLower(string(body))
	switch {
	case status == http.StatusPaymentRequired || strings.Contains(text, "insufficient_quota") ||
		strings.Contains(text, "credit") || strings.Contains(text, "budget exhausted") || strings.Contains(text, "billing"):
		return "The provider reports that billing or available credits need attention."
	case status == http.StatusTooManyRequests:
		return "The model is rate-limited or its quota is exhausted; check the provider's usage and credits."
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "The managed LLM provider rejected this model request; check access or subscription status."
	case status == http.StatusNotFound:
		return "This model or API endpoint was not found. Try the other API style or refresh the model list."
	case status == http.StatusBadRequest:
		return "The model rejected the health-check request. The selected API style may not match this model."
	case status >= 500:
		return "The selected model provider is temporarily unavailable."
	default:
		return fmt.Sprintf("The model check failed with HTTP %d.", status)
	}
}

func hasImageOutput(model, metadata map[string]any) bool {
	for _, root := range []map[string]any{model, metadata, mapValue(metadata["capabilities"])} {
		if root == nil {
			continue
		}
		for key, value := range root {
			name := strings.ToLower(key)
			if (name == "image_output" || name == "image_generation" || name == "generates_images") &&
				boolValue(value) {
				return true
			}
			if name == "modalities" {
				valueMap := mapValue(value)
				if containsImageModality(valueMap["output"]) {
					return true
				}
			}
			if strings.Contains(name, "output") && strings.Contains(name, "modalit") &&
				containsImageModality(value) {
				return true
			}
		}
	}
	return false
}

func containsImageModality(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(strings.ToLower(typed), "image")
	case []any:
		for _, item := range typed {
			if containsImageModality(item) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if strings.EqualFold(key, "output") && containsImageModality(item) {
				return true
			}
		}
	}
	return false
}

func mapValue(value any) map[string]any {
	out, _ := value.(map[string]any)
	return out
}

func firstString(values ...any) string {
	for _, value := range values {
		if text := strings.TrimSpace(stringValue(value)); text != "" {
			return text
		}
	}
	return ""
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func boolValue(value any) bool {
	boolean, _ := value.(bool)
	return boolean
}

func optionalFloat(value any) *float64 {
	var result float64
	switch number := value.(type) {
	case float64:
		result = number
	case json.Number:
		parsed, err := strconv.ParseFloat(number.String(), 64)
		if err != nil {
			return nil
		}
		result = parsed
	default:
		return nil
	}
	if result < 0 || result > 1e6 {
		return nil
	}
	return &result
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
