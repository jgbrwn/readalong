package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	defaultLibriVoxAPI = "https://librivox.org/api/feed/audiobooks/"
	requestGap         = 3 * time.Second
	fallbackSearchTime = 15 * time.Second
	cacheDuration      = 15 * time.Minute
	maxResponseBytes   = 4 << 20
)

type Author struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type Record struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Language        string   `json:"language"`
	URLTextSource   string   `json:"url_text_source"`
	URLZipFile      string   `json:"url_zip_file"`
	URLLibriVox     string   `json:"url_librivox"`
	URLInternetArch string   `json:"url_iarchive"`
	TotalTime       string   `json:"totaltime"`
	TotalTimeSecs   int64    `json:"totaltimesecs"`
	Authors         []Author `json:"authors"`
}

type Response struct {
	Books []Record `json:"books"`
}

type Pair struct {
	RecordID     string   `json:"record_id"`
	Title        string   `json:"title"`
	Authors      []string `json:"authors"`
	Language     string   `json:"language"`
	DurationMS   int64    `json:"duration_ms"`
	LibriVoxURL  string   `json:"librivox_url"`
	GutenbergID  string   `json:"gutenberg_id"`
	GutenbergURL string   `json:"gutenberg_url"`
}

type cacheEntry struct {
	expires time.Time
	records []Record
}

type recordEntry struct {
	expires time.Time
	record  Record
}

// Client uses LibriVox's documented public API. Requests are serialized,
// cached, and spaced apart to be a considerate client of the catalog.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	mu            sync.Mutex
	lastRequest   time.Time
	minRequestGap time.Duration
	queries       map[string]cacheEntry
	byID          map[string]recordEntry
}

func NewClient() *Client {
	return &Client{
		BaseURL:       defaultLibriVoxAPI,
		HTTP:          &http.Client{Timeout: 20 * time.Second},
		minRequestGap: requestGap,
		queries:       make(map[string]cacheEntry),
		byID:          make(map[string]recordEntry),
	}
}

func (c *Client) Search(ctx context.Context, query string) ([]Pair, error) {
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 || len([]rune(query)) > 100 || hasControls(query) {
		return nil, fmt.Errorf("search must be between 2 and 100 characters")
	}
	key := strings.ToLower(query)
	records, err := c.searchRecords(ctx, key, query)
	if err != nil {
		return nil, err
	}
	pairs := pairsForRecords(records)
	if len(pairs) > 0 {
		return pairs, nil
	}
	fallback := fallbackTitleQuery(query)
	if fallback == "" {
		return pairs, nil
	}
	fallbackCtx, cancel := context.WithTimeout(ctx, fallbackSearchTime)
	defer cancel()
	records, err = c.searchRecords(fallbackCtx, "fallback:"+strings.ToLower(fallback), fallback)
	if err != nil {
		return nil, err
	}
	for _, pair := range pairsForRecords(records) {
		if titleContainsQueryTerms(pair.Title, query) {
			pairs = append(pairs, pair)
		}
	}
	return pairs, nil
}

func (c *Client) searchRecords(ctx context.Context, key, query string) ([]Record, error) {
	return c.fetch(ctx, key, url.Values{
		"title":    []string{query},
		"format":   []string{"json"},
		"extended": []string{"1"},
		"limit":    []string{"20"},
	})
}

func pairsForRecords(records []Record) []Pair {
	pairs := make([]Pair, 0, len(records))
	for _, record := range records {
		if pair, ok := ToPair(record); ok {
			pairs = append(pairs, pair)
		}
	}
	return pairs
}

var titleSearchStopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "as": {}, "at": {}, "by": {}, "for": {},
	"from": {}, "in": {}, "into": {}, "of": {}, "on": {}, "or": {}, "the": {},
	"to": {}, "with": {},
}

func searchTokens(value string) []string {
	var tokens []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}

// fallbackTitleQuery chooses one distinctive phrase only when an exact
// multiword title search found no eligible pair. The caller still verifies
// every meaningful input word in the returned title, avoiding broad matches.
func fallbackTitleQuery(query string) string {
	tokens := searchTokens(query)
	if len(tokens) < 3 {
		return ""
	}
	for i := len(tokens) - 1; i > 0; i-- {
		if _, stop := titleSearchStopWords[tokens[i]]; stop {
			continue
		}
		if _, stop := titleSearchStopWords[tokens[i-1]]; stop {
			continue
		}
		return tokens[i-1] + " " + tokens[i]
	}
	for i := len(tokens) - 1; i >= 0; i-- {
		if _, stop := titleSearchStopWords[tokens[i]]; !stop && len([]rune(tokens[i])) >= 4 {
			return tokens[i]
		}
	}
	return ""
}

func titleContainsQueryTerms(title, query string) bool {
	titleTokens := make(map[string]bool)
	for _, token := range searchTokens(title) {
		titleTokens[token] = true
	}
	required := 0
	for _, token := range searchTokens(query) {
		if _, stop := titleSearchStopWords[token]; stop {
			continue
		}
		required++
		if !titleTokens[token] {
			return false
		}
	}
	return required >= 2
}

func (c *Client) ByID(ctx context.Context, id string) (Record, error) {
	if !digitsOnly(id) || len(id) > 12 {
		return Record{}, fmt.Errorf("invalid LibriVox record ID")
	}
	c.mu.Lock()
	if cached, ok := c.byID[id]; ok && time.Now().Before(cached.expires) {
		c.mu.Unlock()
		return cached.record, nil
	}
	delete(c.byID, id)
	c.mu.Unlock()
	records, err := c.fetch(ctx, "id:"+id, url.Values{
		"id":       []string{id},
		"format":   []string{"json"},
		"extended": []string{"1"},
		"limit":    []string{"1"},
	})
	if err != nil {
		return Record{}, err
	}
	for _, record := range records {
		if record.ID == id {
			if _, ok := ToPair(record); !ok {
				return Record{}, fmt.Errorf("this record has no supported paired text source")
			}
			return record, nil
		}
	}
	return Record{}, fmt.Errorf("LibriVox record was not found")
}

func (c *Client) fetch(ctx context.Context, key string, values url.Values) ([]Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.queries == nil {
		c.queries = make(map[string]cacheEntry)
	}
	if c.byID == nil {
		c.byID = make(map[string]recordEntry)
	}
	now := time.Now()
	for cachedKey, entry := range c.queries {
		if !now.Before(entry.expires) {
			delete(c.queries, cachedKey)
		}
	}
	if cached, ok := c.queries[key]; ok && now.Before(cached.expires) {
		return append([]Record(nil), cached.records...), nil
	}
	if len(c.queries) >= 128 {
		var oldestKey string
		oldestExpiry := time.Time{}
		for cachedKey, entry := range c.queries {
			if oldestKey == "" || entry.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = cachedKey, entry.expires
			}
		}
		delete(c.queries, oldestKey)
	}
	if len(c.byID) > 512 {
		clear(c.byID)
	}

	base := c.BaseURL
	if base == "" {
		base = defaultLibriVoxAPI
	}
	u, err := url.Parse(base)
	if err != nil || !validCatalogBase(u) {
		return nil, fmt.Errorf("LibriVox catalog endpoint is invalid")
	}
	u.RawQuery = values.Encode()
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	safeClient := *client
	safeClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 || !sameCatalogOrigin(u, req.URL) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	var status int
	var body []byte
	for attempt := 0; attempt < 2; attempt++ {
		status, body, err = c.requestCatalog(ctx, &safeClient, u)
		if err != nil {
			if attempt == 0 && ctx.Err() == nil {
				continue
			}
			return nil, fmt.Errorf("LibriVox catalog is temporarily unavailable")
		}
		if status == http.StatusOK {
			break
		}
		if status == http.StatusNotFound {
			var apiError struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(body, &apiError) == nil &&
				strings.EqualFold(strings.TrimSpace(apiError.Error), "Audiobooks could not be found") {
				if values.Has("id") {
					return nil, fmt.Errorf("LibriVox record was not found")
				}
				records := []Record{}
				c.queries[key] = cacheEntry{expires: time.Now().Add(cacheDuration), records: records}
				return records, nil
			}
		}
		if attempt == 0 && transientCatalogStatus(status) {
			continue
		}
		if status == http.StatusTooManyRequests {
			return nil, fmt.Errorf("LibriVox catalog is busy; wait a few seconds and try again")
		}
		if status >= 500 {
			return nil, fmt.Errorf("LibriVox catalog is temporarily unavailable (HTTP %d)", status)
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("LibriVox catalog returned HTTP %d", status)
		}
	}
	var result Response
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("LibriVox catalog response was invalid")
	}
	records := make([]Record, 0, len(result.Books))
	for _, record := range result.Books {
		if record.ID == "" || record.Title == "" {
			continue
		}
		c.byID[record.ID] = recordEntry{expires: time.Now().Add(cacheDuration), record: record}
		records = append(records, record)
	}
	c.queries[key] = cacheEntry{expires: time.Now().Add(cacheDuration), records: records}
	return append([]Record(nil), records...), nil
}

func (c *Client) requestCatalog(ctx context.Context, client *http.Client, u *url.URL) (int, []byte, error) {
	if wait := c.minRequestGap - time.Since(c.lastRequest); wait > 0 && !c.lastRequest.IsZero() {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-timer.C:
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, nil, fmt.Errorf("could not create catalog request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Readalong/1.0 (paired public-domain catalog)")
	c.lastRequest = time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, fmt.Errorf("catalog response could not be read")
	}
	if len(body) > maxResponseBytes {
		return 0, nil, fmt.Errorf("catalog response is too large")
	}
	return resp.StatusCode, body, nil
}

func transientCatalogStatus(status int) bool {
	return status >= 500 && status <= 599 &&
		status != http.StatusNotImplemented && status != http.StatusHTTPVersionNotSupported
}

func sameCatalogOrigin(base, target *url.URL) bool {
	if base.Scheme != target.Scheme {
		return false
	}
	baseHost, targetHost := strings.ToLower(base.Hostname()), strings.ToLower(target.Hostname())
	if baseHost == "librivox.org" || baseHost == "www.librivox.org" {
		if targetHost != "librivox.org" && targetHost != "www.librivox.org" {
			return false
		}
	} else if baseHost != targetHost {
		return false
	}
	return effectivePort(base) == effectivePort(target)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func validCatalogBase(u *url.URL) bool {
	if u == nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if (host == "librivox.org" || host == "www.librivox.org") && u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(host)
	return u.Scheme == "http" && (host == "localhost" || ip != nil && ip.IsLoopback())
}

func ToPair(record Record) (Pair, bool) {
	if !digitsOnly(record.ID) || record.Title == "" || len(record.Title) > 300 || !safeLibriVoxURL(record.URLLibriVox) {
		return Pair{}, false
	}
	gutenbergID, ok := gutenbergID(record.URLTextSource)
	if !ok || !safeArchiveURL(record.URLZipFile) {
		return Pair{}, false
	}
	authors := make([]string, 0, len(record.Authors))
	for _, author := range record.Authors {
		name := strings.TrimSpace(strings.Join([]string{author.FirstName, author.LastName}, " "))
		if name != "" && len(name) <= 200 {
			authors = append(authors, name)
		}
	}
	sort.Strings(authors)
	return Pair{
		RecordID: record.ID, Title: record.Title, Authors: authors, Language: record.Language,
		DurationMS: record.TotalTimeSecs * 1000, LibriVoxURL: record.URLLibriVox,
		GutenbergID: gutenbergID, GutenbergURL: "https://www.gutenberg.org/ebooks/" + gutenbergID,
	}, true
}

func gutenbergID(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.RawQuery != "" || u.Fragment != "" ||
		(u.Hostname() != "www.gutenberg.org" && u.Hostname() != "gutenberg.org") {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || (parts[0] != "ebooks" && parts[0] != "etext") || !digitsOnly(parts[1]) {
		return "", false
	}
	return parts[1], true
}

func safeArchiveURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	hostOK := u.Hostname() == "archive.org" || u.Hostname() == "www.archive.org"
	pathOK := strings.HasPrefix(u.Path, "/compress/") || strings.HasPrefix(u.Path, "/download/")
	return hostOK && pathOK
}

func safeLibriVoxURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	return u.Hostname() == "librivox.org" || u.Hostname() == "www.librivox.org"
}

func digitsOnly(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func hasControls(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// GutenbergURL converts a catalog Gutenberg ID into the canonical public page.
func GutenbergURL(id string) (string, error) {
	if !digitsOnly(id) || len(id) > 12 {
		return "", fmt.Errorf("invalid Project Gutenberg ID")
	}
	return "https://www.gutenberg.org/ebooks/" + id, nil
}
