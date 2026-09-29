package groq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Client struct {
	APIKey, Model, Language string
	HTTP                    *http.Client
	Endpoint                string
}
type Word struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}
type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}
type Response struct {
	Text     string    `json:"text"`
	Words    []Word    `json:"words"`
	Segments []Segment `json:"segments"`
	Language string    `json:"language"`
}
type RateLimitError struct {
	RetryAfter time.Duration
	Body       string
}

func (e *RateLimitError) Error() string { return "groq rate limited" }

func (c *Client) TranscribeFile(ctx context.Context, path, prompt string) (Response, error) {
	var out Response
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return out, err
	}
	if _, err = io.Copy(part, f); err != nil {
		return out, err
	}
	fields := map[string]string{"model": c.Model, "response_format": "verbose_json", "timestamp_granularities[]": "word"}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.WriteField("timestamp_granularities[]", "segment")
	if c.Language != "" {
		_ = mw.WriteField("language", c.Language)
	}
	if prompt != "" {
		_ = mw.WriteField("prompt", prompt)
	}
	_ = mw.Close()
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = "https://api.groq.com/openai/v1/audio/transcriptions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &b)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return out, err
	}
	if resp.StatusCode == 429 {
		d := time.Minute
		if s := resp.Header.Get("Retry-After"); s != "" {
			if n, e := strconv.Atoi(s); e == nil {
				d = time.Duration(n) * time.Second
			} else if retryAt, e := http.ParseTime(s); e == nil {
				d = time.Until(retryAt)
			}
		}
		if d < time.Second {
			d = time.Second
		}
		return out, &RateLimitError{RetryAfter: d, Body: string(body)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("groq status %d: %s", resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	return out, nil
}
