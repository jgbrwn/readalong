package httpapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jgbrwn/readalong/internal/coverai"
)

const coverAISettingsKey = "cover_ai_settings"

type coverAISettings = coverai.Settings

type coverAIResponse struct {
	Settings             coverAISettings      `json:"settings"`
	Models               []coverai.ImageModel `json:"models"`
	OpenRouterConfigured bool                 `json:"openrouter_configured"`
	Warning              string               `json:"warning,omitempty"`
}

func (s *Server) getCoverAISettings(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	settings, err := s.readCoverAISettings(r.Context())
	if err != nil {
		http.Error(w, "cover settings could not be loaded", http.StatusInternalServerError)
		return
	}
	response := coverAIResponse{
		Settings: settings, Models: coverai.ImageModels(),
		OpenRouterConfigured: s.coverAI.Configured(),
	}
	if !response.OpenRouterConfigured {
		response.Warning = "Set OPENROUTER_API_KEY in the private .env file and restart Readalong to enable paid image generation."
	}
	jsonOut(w, response)
}

func (s *Server) saveCoverAISettings(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var settings coverAISettings
	if err := decodeAdminJSON(w, r, 4096, &settings); err != nil {
		http.Error(w, "invalid cover settings", http.StatusBadRequest)
		return
	}
	settings.ModelID = strings.TrimSpace(settings.ModelID)
	if settings.ModelID == "" {
		settings.ModelID = coverai.DefaultImageModelID
	}
	if !coverai.IsImageModel(settings.ModelID) {
		http.Error(w, "choose one of the three supported OpenRouter image models", http.StatusBadRequest)
		return
	}
	if settings.Enabled && !s.coverAI.Configured() {
		http.Error(w, "set OPENROUTER_API_KEY in the private .env file and restart Readalong before enabling image generation", http.StatusServiceUnavailable)
		return
	}
	settings = coverai.NormalizeSettings(settings, s.coverAI.Configured())
	payload, err := json.Marshal(settings)
	if err != nil {
		http.Error(w, "cover settings could not be saved", http.StatusInternalServerError)
		return
	}
	if err := s.db.SetAppSetting(r.Context(), coverAISettingsKey, string(payload)); err != nil {
		http.Error(w, "cover settings could not be saved", http.StatusInternalServerError)
		return
	}
	if settings.CatalogLookupEnabled || settings.Enabled {
		if err := s.db.RequeueUnscheduledCoverLookups(r.Context()); err != nil {
			http.Error(w, "cover settings saved, but pending cover work could not be resumed", http.StatusInternalServerError)
			return
		}
	}
	jsonOut(w, map[string]any{"settings": settings})
}

func (s *Server) readCoverAISettings(ctx context.Context) (coverAISettings, error) {
	setting, found, err := s.db.AppSetting(ctx, coverAISettingsKey)
	if err != nil {
		return coverAISettings{}, err
	}
	if !found {
		return coverai.DefaultSettings(s.coverAI.Configured()), nil
	}
	var settings coverAISettings
	if err := json.Unmarshal([]byte(setting.Value), &settings); err != nil {
		return coverAISettings{}, err
	}
	// Old persisted API-style fields are ignored by JSON decoding. Non-image
	// text-model IDs migrate to the new GPT Image 2 picker default.
	return coverai.NormalizeSettings(settings, s.coverAI.Configured()), nil
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request contains trailing data")
		}
		return err
	}
	return nil
}
