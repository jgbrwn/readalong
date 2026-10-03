package httpapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jgbrwn/readalong/internal/coverai"
)

const (
	coverAISettingsKey = "cover_ai_settings"
	coverAIModelsKey   = "cover_ai_models"
	modelCatalogTTL    = 6 * time.Hour
)

type coverModelRegistry interface {
	Discover(context.Context) ([]coverai.Model, error)
	CheckModel(context.Context, string, coverai.APIStyle) (coverai.CheckResult, error)
}

type coverAISettings = coverai.Settings

type cachedCoverModels struct {
	Models    []coverai.Model `json:"models"`
	FetchedAt time.Time       `json:"fetched_at"`
}

type coverAIResponse struct {
	Settings  coverAISettings `json:"settings"`
	Models    []coverai.Model `json:"models"`
	FetchedAt string          `json:"fetched_at,omitempty"`
	Stale     bool            `json:"stale"`
	Warning   string          `json:"warning,omitempty"`
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
	cache, stale, warning, err := s.loadCoverModels(r.Context(), false)
	if err != nil {
		http.Error(w, "model discovery is unavailable; try refreshing the model list", http.StatusBadGateway)
		return
	}
	response := coverAIResponse{Settings: settings, Models: cache.Models, Stale: stale, Warning: warning}
	if !cache.FetchedAt.IsZero() {
		response.FetchedAt = cache.FetchedAt.UTC().Format(time.RFC3339)
	}
	jsonOut(w, response)
}

func (s *Server) refreshCoverAIModels(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	models, err := s.coverAI.Discover(r.Context())
	if err != nil {
		http.Error(w, "could not refresh the managed LLM model list", http.StatusBadGateway)
		return
	}
	cache := cachedCoverModels{Models: models, FetchedAt: time.Now().UTC()}
	if err := s.storeCoverModels(r.Context(), cache); err != nil {
		http.Error(w, "model list could not be cached", http.StatusInternalServerError)
		return
	}
	jsonOut(w, coverAIResponse{Settings: mustCoverAISettings(r.Context(), s), Models: cache.Models,
		FetchedAt: cache.FetchedAt.Format(time.RFC3339)})
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
	if settings.Enabled && settings.ModelID == "" {
		http.Error(w, "choose a vision-capable model before enabling generated covers", http.StatusBadRequest)
		return
	}
	if settings.ModelID != "" {
		if settings.APIStyle != coverai.APIResponses && settings.APIStyle != coverai.APIChat {
			http.Error(w, "choose Responses or Chat Completions for this model", http.StatusBadRequest)
			return
		}
		cache, _, _, err := s.loadCoverModels(r.Context(), false)
		if err != nil || !containsCoverModel(cache.Models, settings.ModelID) {
			http.Error(w, "selected model is not in the current managed model catalog", http.StatusBadRequest)
			return
		}
	} else {
		settings.APIStyle = ""
	}
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

func (s *Server) checkCoverAIModel(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var request struct {
		ModelID  string           `json:"model_id"`
		APIStyle coverai.APIStyle `json:"api_style"`
	}
	if err := decodeAdminJSON(w, r, 4096, &request); err != nil {
		http.Error(w, "invalid model check request", http.StatusBadRequest)
		return
	}
	request.ModelID = strings.TrimSpace(request.ModelID)
	cache, _, _, err := s.loadCoverModels(r.Context(), false)
	if err != nil || !containsCoverModel(cache.Models, request.ModelID) {
		http.Error(w, "select a model from the discovered model list", http.StatusBadRequest)
		return
	}
	now := time.Now()
	s.modelCheckMu.Lock()
	lastCheck := s.modelCheckAt[request.ModelID]
	if !lastCheck.IsZero() && now.Sub(lastCheck) < 15*time.Second {
		s.modelCheckMu.Unlock()
		http.Error(w, "wait a few seconds before checking this model again", http.StatusTooManyRequests)
		return
	}
	s.modelCheckAt[request.ModelID] = now
	s.modelCheckMu.Unlock()
	result, err := s.coverAI.CheckModel(r.Context(), request.ModelID, request.APIStyle)
	if err != nil {
		http.Error(w, "model health check could not run", http.StatusBadGateway)
		return
	}
	jsonOut(w, result)
}

func (s *Server) readCoverAISettings(ctx context.Context) (coverAISettings, error) {
	setting, found, err := s.db.AppSetting(ctx, coverAISettingsKey)
	if err != nil || !found {
		return coverAISettings{
			APIStyle: coverai.APIResponses, CatalogLookupEnabled: true, UseBookDescription: true,
		}, err
	}
	var settings coverAISettings
	if err := json.Unmarshal([]byte(setting.Value), &settings); err != nil {
		return coverAISettings{
			APIStyle: coverai.APIResponses, CatalogLookupEnabled: true, UseBookDescription: true,
		}, nil
	}
	if settings.APIStyle == "" {
		settings.APIStyle = coverai.APIResponses
	}
	return settings, nil
}

func mustCoverAISettings(ctx context.Context, s *Server) coverAISettings {
	settings, err := s.readCoverAISettings(ctx)
	if err != nil {
		return coverAISettings{}
	}
	return settings
}

func (s *Server) loadCoverModels(ctx context.Context, forceRefresh bool) (cachedCoverModels, bool, string, error) {
	var cache cachedCoverModels
	setting, found, err := s.db.AppSetting(ctx, coverAIModelsKey)
	if err != nil {
		return cache, false, "", err
	}
	if found {
		if json.Unmarshal([]byte(setting.Value), &cache) != nil {
			cache = cachedCoverModels{}
		}
	}
	stale := cache.FetchedAt.IsZero() || time.Since(cache.FetchedAt) > modelCatalogTTL
	if !forceRefresh && !stale {
		return cache, false, "", nil
	}
	models, refreshErr := s.coverAI.Discover(ctx)
	if refreshErr == nil {
		cache = cachedCoverModels{Models: models, FetchedAt: time.Now().UTC()}
		if err := s.storeCoverModels(ctx, cache); err != nil {
			return cache, false, "", err
		}
		return cache, false, "", nil
	}
	if len(cache.Models) > 0 {
		return cache, true, "Model discovery is unavailable; showing the last cached list.", nil
	}
	return cache, true, "", refreshErr
}

func (s *Server) storeCoverModels(ctx context.Context, cache cachedCoverModels) error {
	payload, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	return s.db.SetAppSetting(ctx, coverAIModelsKey, string(payload))
}

func containsCoverModel(models []coverai.Model, id string) bool {
	for _, model := range models {
		if model.ID == id && model.Vision {
			return true
		}
	}
	return false
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
