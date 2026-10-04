package coverai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestImageModelPickerHasThreeFixedOptionsAndGPTDefault(t *testing.T) {
	models := ImageModels()
	want := []string{
		"openai/gpt-image-2",
		"bytedance-seed/seedream-4.5",
		"black-forest-labs/flux.2-pro",
	}
	if len(models) != len(want) || DefaultImageModelID != want[0] {
		t.Fatalf("image model picker=%#v default=%q", models, DefaultImageModelID)
	}
	for i, model := range models {
		if model.ID != want[i] || strings.TrimSpace(model.Name) == "" {
			t.Fatalf("picker option %d=%#v, want ID %q", i, model, want[i])
		}
	}
	if IsImageModel("openrouter/openai/gpt-image-2") || IsImageModel("openai/gpt-6.1-sol") {
		t.Fatal("managed-gateway text-model IDs were accepted as direct OpenRouter image models")
	}
}

func TestBuildCoverPromptUsesStoryKnowledgeAndRichBoundedBookContext(t *testing.T) {
	prompt, err := BuildCoverPrompt(CoverDetails{
		Title: "Little Women (dramatic reading)", Author: "Louisa May Alcott",
		Description: "The March sisters—Meg, Jo, Beth, and Amy—grow up together in New England.",
		Year:        1868,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		`"Little Women"`, `"Louisa May Alcott"`, "Verified first-publication year: 1868",
		"The March sisters—Meg, Jo, Beth, and Amy", "four distinct", "not a flower or plant",
		"exact title and author", "portrait 2:3",
	} {
		if !strings.Contains(prompt, text) {
			t.Errorf("prompt omitted %q", text)
		}
	}
	if strings.Contains(prompt, "dramatic reading") || len(prompt) > 12_000 {
		t.Fatalf("production label leaked into cover title or prompt exceeded its cap")
	}
}

func TestOpenRouterImageRequestsUseFixedEndpointAndModelSpecificParameters(t *testing.T) {
	pngData := testPortraitPNG(t, 768, 1152)
	var calls []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/images" || r.Method != http.MethodPost {
			t.Errorf("image API request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-only-key" {
			t.Errorf("authorization header was not scoped to OpenRouter")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		calls = append(calls, request)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{
				"b64_json": base64.StdEncoding.EncodeToString(pngData), "media_type": "image/png",
			}},
			"usage": map[string]any{"cost": 0.04},
		})
	}))
	defer server.Close()

	registry := NewRegistry("test-only-key")
	registry.HTTP = &http.Client{Transport: rewriteTransport{target: server.URL}}
	details := CoverDetails{
		Title: "Little Women (dramatic reading)", Author: "Louisa May Alcott",
		Description: "The March sisters live in nineteenth-century New England.", Year: 1868,
	}
	models := ImageModels()
	for _, model := range models {
		result, err := registry.GenerateCoverImage(context.Background(), model.ID, details)
		if err != nil {
			t.Fatalf("%s generation: %v", model.ID, err)
		}
		if result.Extension != "jpg" || !bytes.HasPrefix(result.Bytes, []byte{0xff, 0xd8, 0xff}) ||
			result.CostUSD == nil || *result.CostUSD != 0.04 {
			t.Fatalf("%s result=%#v", model.ID, result)
		}
		decoded, err := jpeg.DecodeConfig(bytes.NewReader(result.Bytes))
		if err != nil || decoded.Width != 768 || decoded.Height != 1152 {
			t.Fatalf("%s normalized image dimensions=%#v err=%v", model.ID, decoded, err)
		}
	}
	if len(calls) != 3 {
		t.Fatalf("OpenRouter calls=%d, want exactly one per selected model", len(calls))
	}
	for i, model := range models {
		request := calls[i]
		if request["model"] != model.ID || request["n"] != float64(1) || request["aspect_ratio"] != "2:3" ||
			!strings.Contains(stringValue(request["prompt"]), "Meg, Jo, Beth, and Amy") {
			t.Errorf("request for %s has wrong model/count/aspect/story prompt: %#v", model.ID, request)
		}
		switch model.ID {
		case DefaultImageModelID:
			if request["quality"] != "auto" || request["background"] != "opaque" ||
				request["resolution"] != nil || request["output_format"] != nil {
				t.Errorf("GPT Image 2 parameters = %#v", request)
			}
		case "bytedance-seed/seedream-4.5":
			if request["resolution"] != "4K" || request["quality"] != nil || request["output_format"] != nil {
				t.Errorf("Seedream parameters = %#v", request)
			}
		case "black-forest-labs/flux.2-pro":
			if request["output_format"] != "jpeg" || request["resolution"] != nil {
				t.Errorf("FLUX parameters = %#v", request)
			}
		}
	}
}

func TestOpenRouterImageGenerationRejectsMissingKeyUnknownModelAndErrors(t *testing.T) {
	details := CoverDetails{Title: "A Book", Author: "An Author"}
	if _, err := NewRegistry().GenerateCoverImage(context.Background(), DefaultImageModelID, details); err == nil ||
		!strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing key error = %v", err)
	}
	if _, err := NewRegistry("test-only-key").GenerateCoverImage(context.Background(), "openrouter/openai/gpt-image-2", details); err == nil {
		t.Fatal("unsupported model ID was accepted")
	}

	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusTooManyRequests} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"private provider details"}`)
		}))
		registry := NewRegistry("sensitive-test-key")
		registry.HTTP = &http.Client{Transport: rewriteTransport{target: server.URL}}
		_, err := registry.GenerateCoverImage(context.Background(), DefaultImageModelID, details)
		server.Close()
		if err == nil || strings.Contains(err.Error(), "sensitive-test-key") ||
			strings.Contains(err.Error(), "private provider details") {
			t.Fatalf("status %d exposed secret/provider body or succeeded: %v", status, err)
		}
	}
}

func TestNormalizeRasterRejectsVectorMalformedOversizedAndLandscapeAssets(t *testing.T) {
	if _, err := normalizeRasterImage([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "image/svg+xml"); err == nil {
		t.Fatal("SVG model output was accepted")
	}
	if _, err := normalizeRasterImage([]byte("not an image"), "image/png"); err == nil {
		t.Fatal("malformed raster output was accepted")
	}
	if _, err := normalizeRasterImage(testPortraitPNG(t, 1200, 800), "image/png"); err == nil {
		t.Fatal("landscape output was accepted as a book cover")
	}
}

func TestNormalizeSettingsMigratesOldTextModelAndDisablesWithoutKey(t *testing.T) {
	legacy := Settings{Enabled: true, ModelID: "openai/gpt-6.1-sol"}
	got := NormalizeSettings(legacy, false)
	if got.Enabled || got.ModelID != DefaultImageModelID {
		t.Fatalf("legacy settings without direct key normalized to %#v", got)
	}
	withKey := NormalizeSettings(legacy, true)
	if !withKey.Enabled || withKey.ModelID != DefaultImageModelID {
		t.Fatalf("legacy settings with key normalized to %#v", withKey)
	}
}

func testPortraitPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			picture.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 120, A: 255})
		}
	}
	var data bytes.Buffer
	if err := png.Encode(&data, picture); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

type rewriteTransport struct{ target string }

func (r rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	target, err := url.Parse(r.target)
	if err != nil {
		return nil, err
	}
	rewritten := request.Clone(request.Context())
	rewritten.URL.Scheme = target.Scheme
	rewritten.URL.Host = target.Host
	rewritten.Host = target.Host
	return http.DefaultTransport.RoundTrip(rewritten)
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
