package coverai

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoverIncludesTextDesignModelsAcrossManagedProviders(t *testing.T) {
	var modelCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/integrations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"integrations": []any{map[string]any{
					"name": "custom-llm-gateway", "type": "llm",
					"help": fmt.Sprintf("Models: %s/v1/models", "http://"+r.Host),
				}},
			})
		case "/v1/models":
			modelCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{
					"id": "openai/test-vision", "owned_by": "chatgpt",
					"metadata": map[string]any{
						"display_name": "Test vision",
						"capabilities": map[string]any{"vision": true},
						"pricing":      map[string]any{"input_per_million": 0.25, "output_per_million": 0.5},
					},
				},
				map[string]any{"id": "test-vision", "owned_by": "chatgpt"},
				map[string]any{
					"id": "neuralwatt/test-vision", "owned_by": "neuralwatt",
					"metadata": map[string]any{
						"display_name": "Neuralwatt vision",
						"capabilities": map[string]any{"vision": true},
					},
				},
				map[string]any{
					"id": "neuralwatt/test-text", "owned_by": "neuralwatt",
					"metadata": map[string]any{
						"display_name": "Neuralwatt text",
						"capabilities": map[string]any{"vision": false},
					},
				},
				map[string]any{
					"id": "openrouter/test-text", "owned_by": "openrouter",
					"name": "OpenRouter: Text",
					"architecture": map[string]any{
						"modality": "text->text", "input_modalities": []string{"text"},
						"output_modalities": []string{"text"},
					},
					"pricing": map[string]any{"prompt": "0.0000008", "completion": "0.0000016"},
				},
				map[string]any{
					"id": "text-only", "metadata": map[string]any{"capabilities": map[string]any{"vision": false}},
				},
				map[string]any{
					"id": "image-output", "metadata": map[string]any{"capabilities": map[string]any{"image_generation": true}},
				},
				map[string]any{
					"id": "image-input-only", "metadata": map[string]any{
						"capabilities":      map[string]any{"vision": false},
						"input_modalities":  []string{"text", "image"},
						"output_modalities": []string{"text"},
					},
				},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	registry := &Registry{ReflectionURL: server.URL + "/integrations", HTTP: server.Client()}
	models, err := registry.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if modelCalls != 1 || len(models) != 5 {
		t.Fatalf("model list calls=%d models=%#v", modelCalls, models)
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	if got := byID["openai/test-vision"]; got.APIStyle != APIResponses || !got.Vision ||
		got.TextOutputKnown || !got.TextOutput || got.Gateway != "OpenAI via exe.dev" {
		t.Fatalf("Responses model metadata was not retained: %#v", got)
	}
	if _, found := byID["test-vision"]; found {
		t.Fatal("unprefixed OpenAI alias was duplicated beside its canonical ID")
	}
	if got := byID["neuralwatt/test-vision"]; got.APIStyle != APIChat || !got.Vision {
		t.Fatalf("Chat Completions model style was not inferred: %#v", got)
	}
	if got := byID["neuralwatt/test-text"]; !got.TextOutput || got.Vision {
		t.Fatalf("text generation was incorrectly gated on vision: %#v", got)
	}
	if got := byID["neuralwatt/test-text"]; got.TextOutputKnown {
		t.Fatalf("inferred Neuralwatt capability was mislabeled as catalog-advertised: %#v", got)
	}
	if got := byID["openrouter/test-text"]; !got.TextOutput || got.InputPerMillion == nil ||
		math.Abs(*got.InputPerMillion-0.8) > 1e-9 || got.OutputPerMillion == nil ||
		math.Abs(*got.OutputPerMillion-1.6) > 1e-9 {
		t.Fatalf("OpenRouter text modalities/pricing were not normalized: %#v", got)
	}
	if _, found := byID["text-only"]; found {
		t.Fatal("model with no advertised/inferred text capability entered the picker")
	}
	if got, found := byID["image-input-only"]; !found || !got.TextOutput || !got.Vision {
		t.Fatalf("text-output model with image input should remain eligible: %#v", got)
	}
	if _, found := byID["image-output"]; found {
		t.Fatal("image-only model entered a text-to-SVG recipe picker")
	}
}

func TestCheckModelUsesSelectedAPIStyleAndReportsQuotaIssues(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/integrations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"integrations": []any{map[string]any{
					"name": "llm", "type": "llm", "help": fmt.Sprintf("curl %s/v1/models", "http://"+r.Host),
				}},
			})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"id": "openai/test", "owned_by": "chatgpt"},
				map[string]any{
					"id": "neuralwatt/test", "owned_by": "neuralwatt",
					"metadata": map[string]any{
						"capabilities": map[string]any{"vision": false},
						"reasoning":    map[string]any{"mandatory": false, "accepted_efforts": []string{"none"}},
					},
				},
			}})
		case "/v1/responses":
			calls = append(calls, "responses")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
				body["model"] != "openai/test" || body["max_output_tokens"] != float64(maxCheckOutputTokens) ||
				body["store"] != false || body["stream"] != true {
				t.Errorf("bad Responses health check: %#v, err=%v", body, err)
			}
			input := anySlice(body["input"])
			if len(input) != 1 || len(anySlice(mapValue(input[0])["content"])) != 1 {
				t.Errorf("Responses health check must use the typed message-list input: %#v", body["input"])
			}
			w.Header().Set("Content-Type", "text/event-stream")
			recipe := `{"motif":"open_book","colors":["#112233","#445566","#778899"]}`
			delta, _ := json.Marshal(recipe)
			_, _ = w.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":" + string(delta) + "}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"))
		case "/v1/chat/completions":
			calls = append(calls, "chat")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
				body["model"] != "neuralwatt/test" || body["max_tokens"] != float64(maxCheckOutputTokens) ||
				body["reasoning_effort"] != "none" || body["stream"] != false {
				t.Errorf("bad Chat Completions health check: %#v, err=%v", body, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
				"message":       map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"motif":"open_book","colors":["#112233","#445566","#778899"]}`}}},
				"finish_reason": "stop",
			}}})
		case "/v1/chat/completions/quota":
			http.Error(w, `{"error":{"code":"insufficient_quota"}}`, http.StatusPaymentRequired)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	registry := &Registry{ReflectionURL: server.URL + "/integrations", HTTP: server.Client()}
	responses, err := registry.CheckModel(context.Background(), "openai/test", APIAuto)
	if err != nil || !responses.Healthy || responses.APIStyle != APIResponses {
		t.Fatalf("Responses check = %#v, err=%v", responses, err)
	}
	chat, err := registry.CheckModel(context.Background(), "neuralwatt/test", APIAuto)
	if err != nil || !chat.Healthy || chat.APIStyle != APIChat {
		t.Fatalf("Chat Completions check = %#v, err=%v", chat, err)
	}
	if strings.Join(calls, ",") != "responses,chat" {
		t.Fatalf("unexpected API calls: %v", calls)
	}
}

func TestAutoCheckFallsBackOnlyWhenPreferredEndpointRejectsRequest(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/integrations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"integrations": []any{map[string]any{
					"name": "llm", "type": "llm", "help": fmt.Sprintf("curl %s/v1/models", "http://"+r.Host),
				}},
			})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"id": "openai/test", "owned_by": "chatgpt"},
			}})
		case "/v1/responses":
			calls = append(calls, "responses")
			http.NotFound(w, r)
		case "/v1/chat/completions":
			calls = append(calls, "chat")
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
				"message":       map[string]any{"content": `{"motif":"open_book","colors":["#112233","#445566","#778899"]}`},
				"finish_reason": "stop",
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	registry := &Registry{ReflectionURL: server.URL + "/integrations", HTTP: server.Client()}
	result, err := registry.CheckModel(context.Background(), "openai/test", APIAuto)
	if err != nil || !result.Healthy || result.APIStyle != APIChat ||
		!strings.Contains(result.Message, "automatic detection") {
		t.Fatalf("automatic endpoint fallback = %#v, err=%v", result, err)
	}
	if strings.Join(calls, ",") != "responses,chat" {
		t.Fatalf("unexpected endpoint attempts: %v", calls)
	}
}

func TestAutoCheckTriesAlternateAfterEmptySuccessfulResponse(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/integrations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"integrations": []any{map[string]any{
					"name": "llm", "type": "llm", "help": fmt.Sprintf("curl %s/v1/models", "http://"+r.Host),
				}},
			})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"id": "openai/test", "owned_by": "chatgpt"},
			}})
		case "/v1/responses":
			calls = append(calls, "responses")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{}})
		case "/v1/chat/completions":
			calls = append(calls, "chat")
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
				"message":       map[string]any{"content": `{"motif":"open_book","colors":["#112233","#445566","#778899"]}`},
				"finish_reason": "stop",
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	registry := &Registry{ReflectionURL: server.URL + "/integrations", HTTP: server.Client()}
	result, err := registry.CheckModel(context.Background(), "openai/test", APIAuto)
	if err != nil || !result.Healthy || result.APIStyle != APIChat {
		t.Fatalf("empty-response fallback = %#v, err=%v", result, err)
	}
	if strings.Join(calls, ",") != "responses,chat" {
		t.Fatalf("unexpected endpoint attempts: %v", calls)
	}
}

func TestHealthCheckDistinguishesOutputBudgetFromEndpointMismatch(t *testing.T) {
	response := parseModelResponse([]byte(`{"choices":[{"message":{"content":null,"reasoning":"thinking"},"finish_reason":"length"}]}`), APIChat)
	if !response.Incomplete || response.Text != "" {
		t.Fatalf("reasoning-only truncation not recognized: %#v", response)
	}
	streamedFailure := parseModelResponse([]byte(`event: response.failed
data: {"type":"response.failed","response":{"error":{"code":"insufficient_quota","message":"billing needed"}}}

`), APIResponses)
	if !streamedFailure.Failed || !strings.Contains(
		modelCheckFailure(0, []byte(streamedFailure.ErrorHint)), "credits") {
		t.Fatalf("streamed billing failure was not classified: %#v", streamedFailure)
	}
}

func TestModelCheckClassifiesSubscriptionAndQuotaFailures(t *testing.T) {
	if got := modelCheckFailure(http.StatusPaymentRequired, []byte(`{"error":"payment required"}`)); !strings.Contains(got, "credits") {
		t.Fatalf("payment failure was not actionable: %q", got)
	}
	if got := modelCheckFailure(http.StatusTooManyRequests, []byte(`{"error":"rate limit"}`)); !strings.Contains(got, "quota") {
		t.Fatalf("rate limit failure was not actionable: %q", got)
	}
	if got := modelCheckFailure(http.StatusTooManyRequests,
		[]byte(`{"metadata":{"raw":"model is temporarily rate-limited upstream","limit_source":"upstream_provider_shared_pool"}}`)); !strings.Contains(got, "upstream provider is temporarily rate-limited") {
		t.Fatalf("shared-pool provider limit was not distinguished: %q", got)
	}
}

func TestGeneratedCoverUsesSafeLocalSVGAndExactTypography(t *testing.T) {
	cover := renderCoverSVG(`<The Book>`, `A & B`, 1935, coverRecipe{
		Motif: "four_sisters", Colors: []string{"#112233", "#445566", "#778899"},
	})
	text := string(cover)
	for _, want := range []string{
		`&lt;The Book&gt;`, `A &amp; B`, "First published 1935",
		`<svg xmlns="http://www.w3.org/2000/svg"`, `READALONG · FOUR SISTERS`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated SVG missing %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"<script", "foreignObject", "javascript:", "https://"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("generated SVG contains unsafe content %q", forbidden)
		}
	}
	if got := strings.Count(text, `class="sister"`); got != 4 {
		t.Fatalf("Little Women motif has %d sisters, want four", got)
	}
}

func TestEveryWhitelistedMotifRendersWellFormedSafeSVG(t *testing.T) {
	for motif := range allowedCoverMotifs {
		svg := renderCoverSVG("A Literary Work", "An Author", 0, coverRecipe{
			Motif: motif, Colors: []string{"#263F39", "#B77D55", "#E3CFA6"},
		})
		var document struct {
			XMLName xml.Name `xml:"svg"`
		}
		if err := xml.Unmarshal(svg, &document); err != nil || document.XMLName.Local != "svg" {
			t.Errorf("motif %q rendered invalid SVG: %v", motif, err)
		}
		if strings.Contains(string(svg), "<script") || strings.Contains(string(svg), "https://") {
			t.Errorf("motif %q rendered unsafe SVG content", motif)
		}
	}
}

func TestParseRecipeFallsBackFromUntrustedModelMarkup(t *testing.T) {
	recipe := parseRecipe(`{"motif":"<svg onload=alert(1)>","colors":["url(x)","#112233","#445566"]}`, "Example")
	if !allowedCoverMotifs[recipe.Motif] || len(recipe.Colors) != 3 {
		t.Fatalf("unsafe model recipe was not replaced: %#v", recipe)
	}
	for _, color := range recipe.Colors {
		if !colorPattern.MatchString(color) {
			t.Fatalf("unsafe color reached renderer: %q", color)
		}
	}
}

func TestGenerateVectorCoverUsesTypedRequestsAndSharedResponseParsing(t *testing.T) {
	var endpoints []string
	var sawLittleWomenPrompt bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/integrations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"integrations": []any{map[string]any{
					"name": "llm", "type": "llm", "help": fmt.Sprintf("curl %s/v1/models", "http://"+r.Host),
				}},
			})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{
					"id": "sample/vision", "owned_by": "neuralwatt",
					"metadata": map[string]any{
						"capabilities": map[string]any{"vision": true},
						"reasoning":    map[string]any{"accepted_efforts": []string{"none"}},
					},
				},
			}})
		case "/v1/responses":
			endpoints = append(endpoints, "responses")
			var request map[string]any
			err := json.NewDecoder(r.Body).Decode(&request)
			input := anySlice(request["input"])
			content := anySlice(mapValue(input[0])["content"])
			prompt := stringValue(mapValue(content[0])["text"])
			if err != nil || request["store"] != false || request["stream"] != true ||
				request["max_output_tokens"] != float64(maxDesignOutputTokens) ||
				!strings.Contains(prompt, "A short story of courage") {
				t.Errorf("Responses cover request omitted the short description: %#v err=%v", request, err)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"{\\\"motif\\\":\\\"moon\\\",\\\"colors\\\":[\\\"#112233\\\",\\\"#445566\\\",\\\"#778899\\\"]}\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"))
		case "/v1/chat/completions":
			endpoints = append(endpoints, "chat")
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("invalid Chat Completions cover request: %v", err)
			}
			messages, _ := request["messages"].([]any)
			prompt := ""
			if len(messages) > 0 {
				prompt = stringValue(mapValue(messages[0])["content"])
			}
			if !strings.Contains(prompt, "A short story of courage") && !strings.Contains(prompt, "Little Women") {
				t.Errorf("Chat Completions cover request omitted expected subject context: %#v", request)
			}
			if strings.Contains(prompt, "Little Women") {
				sawLittleWomenPrompt = strings.Contains(prompt, `The story title is "Little Women"`) &&
					strings.Contains(prompt, "four distinct young women/sisters") &&
					strings.Contains(prompt, "not flowers or plants")
			}
			if prompt == "" {
				t.Errorf("Chat Completions cover request omitted the short description: %#v", request)
			}
			recipe := `{"motif":"ship","colors":["#123456","#456789","#789ABC"]}`
			if strings.Contains(prompt, "Little Women") {
				recipe = `{"motif":"four_sisters","colors":["#123456","#456789","#789ABC"]}`
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
				"message":       map[string]any{"content": recipe},
				"finish_reason": "stop",
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	registry := &Registry{ReflectionURL: server.URL + "/integrations", HTTP: server.Client()}
	for _, style := range []APIStyle{APIResponses, APIChat} {
		image, err := registry.GenerateVectorCover(context.Background(), "sample/vision", style,
			"Sample Title", "An Author", "A short story of courage.", 2001)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(image), "First published 2001") ||
			!strings.Contains(string(image), "Sample Title") {
			t.Fatalf("cover renderer lost exact metadata for %s", style)
		}
	}
	littleWomen, err := registry.GenerateVectorCover(context.Background(), "sample/vision", APIChat,
		"Little Women (dramatic reading)", "Louisa May Alcott", "", 1868)
	if err != nil {
		t.Fatal(err)
	}
	if !sawLittleWomenPrompt || !strings.Contains(string(littleWomen), "FOUR SISTERS") ||
		strings.Count(string(littleWomen), `class="sister"`) != 4 {
		t.Fatalf("Little Women title-specific illustration was not generated")
	}
	if strings.Join(endpoints, ",") != "responses,chat,chat" {
		t.Fatalf("cover requests used unexpected endpoints: %v", endpoints)
	}
}
