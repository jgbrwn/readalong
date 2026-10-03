package coverai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoverUsesReflectionNameAndAdvertisedCapabilities(t *testing.T) {
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
				map[string]any{
					"id": "neuralwatt/test-vision", "owned_by": "neuralwatt",
					"metadata": map[string]any{
						"display_name": "Neuralwatt vision",
						"capabilities": map[string]any{"vision": true},
					},
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
	if modelCalls != 1 || len(models) != 3 {
		t.Fatalf("model list calls=%d models=%#v", modelCalls, models)
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	if got := byID["openai/test-vision"]; got.APIStyle != APIResponses || !got.Vision ||
		got.InputPerMillion == nil || *got.InputPerMillion != 0.25 {
		t.Fatalf("Responses model metadata was not retained: %#v", got)
	}
	if got := byID["neuralwatt/test-vision"]; got.APIStyle != APIChat || !got.Vision {
		t.Fatalf("Chat Completions model style was not inferred: %#v", got)
	}
	if got := byID["image-output"]; !got.ImageOutput || got.Vision {
		t.Fatalf("explicit image-output capability was lost: %#v", got)
	}
	if _, found := byID["text-only"]; found {
		t.Fatal("text-only model entered the cover model picker")
	}
	if _, found := byID["image-input-only"]; found {
		t.Fatal("image-input capability was mistaken for image output")
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
		case "/v1/responses":
			calls = append(calls, "responses")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
				body["model"] != "openai/test" || body["max_output_tokens"] != float64(8) {
				t.Errorf("bad Responses health check: %#v, err=%v", body, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"output": []any{map[string]any{
				"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "OK"}},
			}}})
		case "/v1/chat/completions":
			calls = append(calls, "chat")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
				body["model"] != "neuralwatt/test" || body["max_tokens"] != float64(8) {
				t.Errorf("bad Chat Completions health check: %#v, err=%v", body, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
				"message": map[string]any{"content": "OK"},
			}}})
		case "/v1/chat/completions/quota":
			http.Error(w, `{"error":{"code":"insufficient_quota"}}`, http.StatusPaymentRequired)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	registry := &Registry{ReflectionURL: server.URL + "/integrations", HTTP: server.Client()}
	responses, err := registry.CheckModel(context.Background(), "openai/test", APIResponses)
	if err != nil || !responses.Healthy || responses.APIStyle != APIResponses {
		t.Fatalf("Responses check = %#v, err=%v", responses, err)
	}
	chat, err := registry.CheckModel(context.Background(), "neuralwatt/test", APIChat)
	if err != nil || !chat.Healthy || chat.APIStyle != APIChat {
		t.Fatalf("Chat Completions check = %#v, err=%v", chat, err)
	}
	if strings.Join(calls, ",") != "responses,chat" {
		t.Fatalf("unexpected API calls: %v", calls)
	}
}

func TestModelCheckClassifiesSubscriptionAndQuotaFailures(t *testing.T) {
	if got := modelCheckFailure(http.StatusPaymentRequired, []byte(`{"error":"payment required"}`)); !strings.Contains(got, "credits") {
		t.Fatalf("payment failure was not actionable: %q", got)
	}
	if got := modelCheckFailure(http.StatusTooManyRequests, []byte(`{"error":"rate limit"}`)); !strings.Contains(got, "quota") {
		t.Fatalf("rate limit failure was not actionable: %q", got)
	}
}

func TestGeneratedCoverUsesSafeLocalSVGAndExactTypography(t *testing.T) {
	cover := renderCoverSVG(`<The Book>`, `A & B`, 1935, coverRecipe{
		Theme: "botanical", Colors: []string{"#112233", "#445566", "#778899"},
	})
	text := string(cover)
	for _, want := range []string{
		`&lt;The Book&gt;`, `A &amp; B`, "First published 1935",
		`<svg xmlns="http://www.w3.org/2000/svg"`, `<path d="M410 490`,
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
}

func TestParseRecipeFallsBackFromUntrustedModelMarkup(t *testing.T) {
	recipe := parseRecipe(`{"theme":"<svg onload=alert(1)>","colors":["url(x)","#112233","#445566"]}`, "Example")
	if !allowedCoverThemes[recipe.Theme] || len(recipe.Colors) != 3 {
		t.Fatalf("unsafe model recipe was not replaced: %#v", recipe)
	}
	for _, color := range recipe.Colors {
		if !colorPattern.MatchString(color) {
			t.Fatalf("unsafe color reached renderer: %q", color)
		}
	}
}

func TestGenerateVectorCoverUsesConfiguredResponsesOrChatEndpoint(t *testing.T) {
	var endpoints []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/integrations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"integrations": []any{map[string]any{
					"name": "llm", "type": "llm", "help": fmt.Sprintf("curl %s/v1/models", "http://"+r.Host),
				}},
			})
		case "/v1/responses":
			endpoints = append(endpoints, "responses")
			var request map[string]any
			err := json.NewDecoder(r.Body).Decode(&request)
			prompt, _ := request["input"].(string)
			if err != nil || !strings.Contains(prompt, "A short story of courage") {
				t.Errorf("Responses cover request omitted the short description: %#v err=%v", request, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"output": []any{map[string]any{
				"type": "message", "content": []any{map[string]any{"type": "output_text",
					"text": `{"theme":"celestial","colors":["#112233","#445566","#778899"]}`}},
			}}})
		case "/v1/chat/completions":
			endpoints = append(endpoints, "chat")
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("invalid Chat Completions cover request: %v", err)
			}
			messages, _ := request["messages"].([]any)
			if len(messages) == 0 || !strings.Contains(stringValue(mapValue(messages[0])["content"]), "A short story of courage") {
				t.Errorf("Chat Completions cover request omitted the short description: %#v", request)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
				"message": map[string]any{"content": `{"theme":"coastal","colors":["#123456","#456789","#789ABC"]}`},
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
	if strings.Join(endpoints, ",") != "responses,chat" {
		t.Fatalf("cover requests used unexpected endpoints: %v", endpoints)
	}
}
