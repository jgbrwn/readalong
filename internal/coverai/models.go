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
	APIAuto      APIStyle = "auto"
	APIResponses APIStyle = "responses"
	APIChat      APIStyle = "chat_completions"
)

const (
	maxCheckOutputTokens   = 128
	maxDesignOutputTokens  = 512
	CurrentSettingsVersion = 1
)

type Settings struct {
	Enabled              bool     `json:"enabled"`
	CatalogLookupEnabled bool     `json:"catalog_lookup_enabled"`
	UseBookDescription   bool     `json:"use_book_description"`
	ModelID              string   `json:"model_id,omitempty"`
	APIStyle             APIStyle `json:"api_style,omitempty"`
	APIStyleVersion      int      `json:"api_style_version,omitempty"`
}

// Model is the browser-safe subset of the managed model catalog used for cover
// design. Text output is what the recipe-based SVG renderer needs; vision is
// only image input and is not required.
type Model struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Gateway             string   `json:"gateway,omitempty"`
	Provider            string   `json:"provider,omitempty"`
	Description         string   `json:"description,omitempty"`
	APIStyle            APIStyle `json:"api_style"`
	TextOutput          bool     `json:"text_output"`
	TextOutputKnown     bool     `json:"text_output_known"`
	Vision              bool     `json:"vision"`
	ImageOutput         bool     `json:"image_output"`
	ReasoningEffortNone bool     `json:"reasoning_effort_none,omitempty"`
	InputPerMillion     *float64 `json:"input_per_million,omitempty"`
	OutputPerMillion    *float64 `json:"output_per_million,omitempty"`
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

// Discover resolves the attached LLM integration through Reflection and
// returns models that can produce text for Readalong's local SVG renderer.
// Catalog capability claims are preserved; metadata-less conversational
// OpenAI models are included as unverified candidates. Discovery never invokes
// a model.
func (r *Registry) Discover(ctx context.Context) ([]Model, error) {
	modelsURL, err := r.resolveModelsURL(ctx)
	if err != nil {
		return nil, err
	}
	return r.discoverAt(ctx, modelsURL)
}

func (r *Registry) discoverAt(ctx context.Context, modelsURL *url.URL) ([]Model, error) {
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
		if !ok || !model.TextOutput {
			continue
		}
		if _, duplicate := seen[model.ID]; duplicate {
			continue
		}
		seen[model.ID] = struct{}{}
		models = append(models, model)
	}
	openAIAliases := make(map[string]bool)
	for _, model := range models {
		if model.Gateway == "OpenAI via exe.dev" && strings.HasPrefix(model.ID, "openai/") {
			openAIAliases[strings.TrimPrefix(model.ID, "openai/")] = true
		}
	}
	if len(openAIAliases) > 0 {
		filtered := models[:0]
		for _, model := range models {
			if model.Gateway == "OpenAI via exe.dev" && !strings.HasPrefix(model.ID, "openai/") &&
				openAIAliases[model.ID] {
				continue
			}
			filtered = append(filtered, model)
		}
		models = filtered
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
	if style != APIAuto && style != APIResponses && style != APIChat {
		return CheckResult{}, fmt.Errorf("select a supported API style")
	}
	modelsURL, model, err := r.lookupModel(ctx, modelID)
	if err != nil {
		return CheckResult{}, err
	}

	started := time.Now()
	styles := apiStyleAttempts(model, style)
	for index, attemptStyle := range styles {
		responseBody, status, err := r.requestText(ctx, modelsURL, model, attemptStyle,
			`Return only this valid design JSON: {"theme":"geometric","colors":["#112233","#445566","#778899"]}.`, maxCheckOutputTokens)
		if err != nil {
			return CheckResult{
				APIStyle: attemptStyle, LatencyMS: time.Since(started).Milliseconds(),
				Message: "The managed LLM service could not be reached.",
			}, nil
		}
		if status >= 200 && status < 300 {
			response := parseModelResponse(responseBody, attemptStyle)
			result := CheckResult{
				APIStyle: attemptStyle, LatencyMS: time.Since(started).Milliseconds(),
			}
			switch {
			case response.Refused:
				result.Message = "The endpoint worked, but the model refused the small test prompt."
			case response.Incomplete:
				result.Message = "The endpoint worked, but the response hit its output budget before completing; this is not an API-style mismatch."
			case response.Failed:
				result.Message = modelCheckFailure(0, []byte(response.ErrorHint))
			case strings.TrimSpace(response.Text) == "" || !hasValidRecipe(response.Text):
				if style == APIAuto && index+1 < len(styles) {
					continue
				}
				if strings.TrimSpace(response.Text) == "" {
					result.Message = "The endpoint accepted the request but returned no user-visible text."
				} else {
					result.Message = "The model returned text, but not a valid cover design recipe."
				}
			default:
				result.Healthy = true
				if index > 0 {
					result.Message = "Model returned a valid cover design recipe; automatic detection selected " + apiStyleLabel(attemptStyle) + "."
				} else if style == APIAuto {
					result.Message = "Model returned a valid cover design recipe via " + apiStyleLabel(attemptStyle) + "."
				} else {
					result.Message = "Model returned a valid cover design recipe; the health check succeeded."
				}
			}
			return result, nil
		}

		if style == APIAuto && index+1 < len(styles) && shouldTryAlternateAPI(status, responseBody) {
			continue
		}
		return CheckResult{
			APIStyle: attemptStyle, LatencyMS: time.Since(started).Milliseconds(),
			Message: modelCheckFailure(status, responseBody),
		}, nil
	}
	return CheckResult{
		LatencyMS: time.Since(started).Milliseconds(),
		Message:   "Neither compatible model API endpoint accepted the health check.",
	}, nil
}

func (r *Registry) lookupModel(ctx context.Context, modelID string) (*url.URL, Model, error) {
	modelsURL, err := r.resolveModelsURL(ctx)
	if err != nil {
		return nil, Model{}, err
	}
	models, err := r.discoverAt(ctx, modelsURL)
	if err != nil {
		return nil, Model{}, err
	}
	for _, model := range models {
		if model.ID == modelID {
			return modelsURL, model, nil
		}
	}
	return nil, Model{}, fmt.Errorf("selected model is not available in the managed model catalog")
}

func apiStyleAttempts(model Model, requested APIStyle) []APIStyle {
	if requested == APIResponses || requested == APIChat {
		return []APIStyle{requested}
	}
	preferred := model.APIStyle
	if preferred != APIResponses && preferred != APIChat {
		preferred = APIChat
	}
	alternate := APIChat
	if preferred == APIChat {
		alternate = APIResponses
	}
	return []APIStyle{preferred, alternate}
}

func apiStyleLabel(style APIStyle) string {
	if style == APIResponses {
		return "Responses API"
	}
	return "Chat Completions API"
}

func shouldTryAlternateAPI(status int, body []byte) bool {
	if status == http.StatusNotFound || status == http.StatusBadRequest {
		return true
	}
	text := strings.ToLower(string(body))
	return status == http.StatusUnprocessableEntity &&
		(strings.Contains(text, "unsupported endpoint") || strings.Contains(text, "unsupported api"))
}

func (r *Registry) requestText(ctx context.Context, modelsURL *url.URL, model Model, style APIStyle,
	prompt string, maxTokens int,
) ([]byte, int, error) {
	payload := map[string]any{"model": model.ID}
	if style == APIResponses {
		endpoint := *modelsURL
		endpoint.Path = "/v1/responses"
		payload["input"] = []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text", "text": prompt,
			}},
		}}
		payload["max_output_tokens"] = maxTokens
		payload["store"] = false
		payload["stream"] = true
		if model.ReasoningEffortNone {
			payload["reasoning"] = map[string]string{"effort": "none"}
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, fmt.Errorf("could not prepare model request")
		}
		return r.do(ctx, http.MethodPost, endpoint.String(), body, modelsURL.Hostname())
	}
	if style != APIChat {
		return nil, 0, fmt.Errorf("unsupported model API style")
	}
	endpoint := *modelsURL
	endpoint.Path = "/v1/chat/completions"
	payload["messages"] = []any{map[string]string{"role": "user", "content": prompt}}
	payload["max_tokens"] = maxTokens
	payload["stream"] = false
	if model.ReasoningEffortNone {
		payload["reasoning_effort"] = "none"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("could not prepare model request")
	}
	return r.do(ctx, http.MethodPost, endpoint.String(), body, modelsURL.Hostname())
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
	req.Header.Set("Accept", "application/json, text/event-stream")
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
	owner := strings.ToLower(stringValue(value["owned_by"]))
	textOutput, textOutputKnown := modelTextOutput(value, metadata, capabilities)
	// The exe.dev catalog exposes the ChatGPT-backed OpenAI routes as
	// conversational model IDs but omits their modality metadata. Include
	// these as candidates and require an explicit runtime health check.
	if !textOutputKnown && owner == "chatgpt" && !nonTextModelID(id) {
		textOutput = true
	}
	// Neuralwatt's models are OpenAI-compatible completion endpoints. Its
	// catalog advertises image input but not text output, so infer text output
	// from this gateway's model type rather than requiring vision. Keep that
	// inference visibly separate from explicitly advertised text capability.
	if !textOutputKnown && owner == "neuralwatt" && !nonTextModelID(id) {
		textOutput = true
	}
	name := firstString(
		metadata["display_name"], value["name"], value["canonical_slug"], id,
	)
	provider := firstString(metadata["provider"])
	gateway := owner
	switch owner {
	case "chatgpt":
		gateway = "OpenAI via exe.dev"
		if provider == "" {
			provider = "OpenAI"
		}
	case "neuralwatt":
		gateway = "Neuralwatt"
		if provider == "" {
			provider = "Neuralwatt"
		}
	case "openrouter":
		gateway = "OpenRouter"
		if provider == "" {
			provider = openRouterProvider(id)
		}
	default:
		if gateway == "" {
			gateway = firstString(metadata["provider"], value["owned_by"])
		}
		if provider == "" {
			provider = gateway
		}
	}
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
		ID:                  id,
		Name:                name,
		Gateway:             gateway,
		Provider:            provider,
		Description:         description,
		APIStyle:            inferAPIStyle(value, metadata, gateway, id),
		TextOutput:          textOutput,
		TextOutputKnown:     textOutputKnown,
		Vision:              boolValue(capabilities["vision"]) || hasImageInput(value, metadata),
		ImageOutput:         hasImageOutput(value, metadata),
		ReasoningEffortNone: supportsNoReasoning(value, metadata),
	}
	if pricing := mapValue(metadata["pricing"]); pricing != nil {
		model.InputPerMillion = optionalFloat(pricing["input_per_million"])
		model.OutputPerMillion = optionalFloat(pricing["output_per_million"])
	}
	if model.InputPerMillion == nil || model.OutputPerMillion == nil {
		if pricing := mapValue(value["pricing"]); pricing != nil {
			if model.InputPerMillion == nil {
				model.InputPerMillion = perTokenPrice(pricing["prompt"])
			}
			if model.OutputPerMillion == nil {
				model.OutputPerMillion = perTokenPrice(pricing["completion"])
			}
		}
	}
	if deprecated, _ := metadata["deprecated"].(bool); deprecated {
		return Model{}, false
	}
	return model, model.TextOutput
}

func hasImageInput(model, metadata map[string]any) bool {
	for _, root := range []map[string]any{model, metadata, mapValue(model["architecture"]), mapValue(metadata["architecture"])} {
		if root == nil {
			continue
		}
		for _, key := range []string{"input_modalities", "input_modality"} {
			if containsImageModality(root[key]) {
				return true
			}
		}
		if modalities := mapValue(root["modalities"]); containsImageModality(modalities["input"]) {
			return true
		}
		if modality, ok := root["modality"].(string); ok {
			if input, _, found := strings.Cut(modality, "->"); found &&
				strings.Contains(strings.ToLower(input), "image") {
				return true
			}
		}
	}
	return false
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

func modelTextOutput(model, metadata, capabilities map[string]any) (bool, bool) {
	roots := []map[string]any{
		model, metadata, capabilities,
		mapValue(model["architecture"]), mapValue(metadata["architecture"]),
	}
	for _, root := range roots {
		if root == nil {
			continue
		}
		for _, key := range []string{"output_modalities", "output_modality"} {
			if modalities, ok := modalitySet(root[key]); ok {
				return modalities["text"], true
			}
		}
		if modalities, ok := modalitySet(root["modalities"]); ok {
			return modalities["text"], true
		}
		if modality, ok := root["modality"].(string); ok {
			_, output, found := strings.Cut(modality, "->")
			if found {
				return strings.Contains(strings.ToLower(output), "text"), true
			}
		}
	}
	for _, root := range roots {
		if root == nil {
			continue
		}
		for _, key := range []string{"text_output", "text_generation", "generates_text"} {
			if value, exists := root[key]; exists {
				return boolValue(value), true
			}
		}
	}
	return false, false
}

func modalitySet(value any) (map[string]bool, bool) {
	modalities := make(map[string]bool)
	switch typed := value.(type) {
	case string:
		for _, part := range strings.FieldsFunc(strings.ToLower(typed), func(r rune) bool {
			return r == ',' || r == '+' || r == ' ' || r == '/'
		}) {
			modalities[part] = true
		}
		return modalities, len(modalities) > 0
	case []any:
		for _, item := range typed {
			if part, ok := item.(string); ok && strings.TrimSpace(part) != "" {
				modalities[strings.ToLower(strings.TrimSpace(part))] = true
			}
		}
		return modalities, len(modalities) > 0
	case map[string]any:
		for key, enabled := range typed {
			if boolValue(enabled) {
				modalities[strings.ToLower(key)] = true
			}
		}
		return modalities, len(modalities) > 0
	default:
		return nil, false
	}
}

func nonTextModelID(id string) bool {
	id = strings.ToLower(id)
	for _, marker := range []string{"embed", "moderation", "whisper", "transcri", "text-to-speech", "tts", "dall-e", "gpt-image"} {
		if strings.Contains(id, marker) {
			return true
		}
	}
	return false
}

func openRouterProvider(id string) string {
	parts := strings.Split(strings.TrimPrefix(id, "openrouter/"), "/")
	if len(parts) > 1 && parts[0] != "" {
		return parts[0]
	}
	return "OpenRouter"
}

func supportsNoReasoning(model, metadata map[string]any) bool {
	for _, root := range []map[string]any{model, metadata} {
		reasoning := mapValue(root["reasoning"])
		if reasoning == nil {
			continue
		}
		if boolValue(reasoning["mandatory"]) {
			continue
		}
		for _, key := range []string{"supported_efforts", "accepted_efforts"} {
			if efforts, ok := reasoning[key].([]any); ok {
				for _, effort := range efforts {
					if strings.EqualFold(stringValue(effort), "none") {
						return true
					}
				}
			}
		}
	}
	return false
}

func perTokenPrice(value any) *float64 {
	price := optionalFloat(value)
	if price == nil || *price > 1 {
		return nil
	}
	perMillion := *price * 1_000_000
	return &perMillion
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
	case strings.Contains(text, "upstream_provider_shared_pool") ||
		strings.Contains(text, "temporarily rate-limited upstream"):
		return "This model's upstream provider is temporarily rate-limited; try again later or choose another model."
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
	case status <= 0:
		return "The model response ended with an error; check provider access, billing, and model availability."
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
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
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
