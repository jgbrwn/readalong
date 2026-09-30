package catalog

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	defaultArchiveBase      = "https://archive.org"
	defaultGutenbergCatalog = "https://www.gutenberg.org/cache/epub/feeds/pg_catalog.csv.gz"
	archiveRecordPrefix     = "ia-"
	archiveCacheDuration    = 15 * time.Minute
	archiveRequestGap       = 3 * time.Second
	archiveSearchTimeout    = 15 * time.Second
	gutenbergCacheTTL       = 7 * 24 * time.Hour
	maxArchiveResponse      = 8 << 20
	maxArchiveQueryCache    = 32
	maxPGCatalogGzip        = 16 << 20
	maxPGCatalogCSV         = 128 << 20
	maxPGCatalogRows        = 200_000
)

var (
	gutenbergURLPattern  = regexp.MustCompile(`(?i)https?://(?:www\.)?gutenberg\.org/(?:ebooks|etext)/([0-9]{1,12})`)
	gutenbergTextPattern = regexp.MustCompile(`(?i)gutenberg\s+e[- ]?text\s*#?\s*([0-9]{1,12})`)
	versionSuffixPattern = regexp.MustCompile(`(?i)\s*\(version\s+[0-9]+[^)]*\)`)
	dramaSuffixPattern   = regexp.MustCompile(`(?i)\s*\((?:dramatic|dramatized)\s+reading\)\s*$`)
	bylineSuffixPattern  = regexp.MustCompile(`(?i),?\s+by\s+.+$`)
	htmlTagPattern       = regexp.MustCompile(`(?s)<[^>]*>`)
	narratorPattern      = regexp.MustCompile(`(?i)\bread\s+(?:in\s+[[:alpha:] -]+\s+)?by\s+([^.;\n]+)`)
	lvPageURLPattern     = regexp.MustCompile(`(?i)https?://(?:www\.)?librivox\.org/[a-z0-9/_-]+`)
)

type iaStrings []string

func (s *iaStrings) UnmarshalJSON(data []byte) error {
	var one string
	if json.Unmarshal(data, &one) == nil {
		*s = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err == nil {
		*s = many
		return nil
	}
	*s = nil
	return nil
}

func (s iaStrings) first() string {
	for _, value := range s {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

type archiveSearchResponse struct {
	Response struct {
		Docs []archiveSearchDoc `json:"docs"`
	} `json:"response"`
}

type archiveSearchDoc struct {
	Identifier  string    `json:"identifier"`
	Title       string    `json:"title"`
	Creator     iaStrings `json:"creator"`
	Source      iaStrings `json:"source"`
	Description iaStrings `json:"description"`
	Language    iaStrings `json:"language"`
	Runtime     iaStrings `json:"runtime"`
	Date        iaStrings `json:"date"`
}

type archiveMetadataResponse struct {
	Metadata archiveItemMetadata `json:"metadata"`
	Files    []archiveFile       `json:"files"`
}

type archiveItemMetadata struct {
	Identifier  string    `json:"identifier"`
	Title       string    `json:"title"`
	Creator     iaStrings `json:"creator"`
	Source      iaStrings `json:"source"`
	Description iaStrings `json:"description"`
	Language    iaStrings `json:"language"`
	Runtime     iaStrings `json:"runtime"`
	Date        iaStrings `json:"date"`
	MediaType   iaStrings `json:"mediatype"`
	Collection  iaStrings `json:"collection"`
}

type archiveFile struct {
	Name    string          `json:"name"`
	Format  iaStrings       `json:"format"`
	Private json.RawMessage `json:"private"`
}

type gutenbergIndex struct {
	fetchedAt time.Time
	byID      map[string]gutenbergEntry
	entries   []gutenbergEntry
}

type gutenbergEntry struct {
	ID       string
	Type     string
	Title    string
	Authors  string
	Language string
	Issued   string
}

// EnableArchiveSearch opts the production catalog client into the Internet
// Archive LibriVox collection as its primary search, with LibriVox API as a
// fallback. Tests can keep using the LibriVox client in isolation.
func (c *Client) EnableArchiveSearch() {
	c.ArchiveEnabled = true
}

func (c *Client) searchArchive(ctx context.Context, query string) ([]Pair, error) {
	records, err := c.fetchArchiveSearch(ctx, query)
	if err != nil {
		return nil, err
	}
	pairs, err := c.archivePairs(ctx, records)
	if err != nil {
		return nil, err
	}
	if len(pairs) > 0 {
		return pairs, nil
	}

	fallback := fallbackTitleQuery(query)
	if fallback == "" {
		return []Pair{}, nil
	}
	records, err = c.fetchArchiveSearch(ctx, fallback)
	if err != nil {
		return nil, err
	}
	pairs, err = c.archivePairs(ctx, records)
	if err != nil {
		return nil, err
	}
	filtered := make([]Pair, 0, len(pairs))
	for _, pair := range pairs {
		if archiveTitleContainsQueryTerms(pair.Title, query) {
			filtered = append(filtered, pair)
		}
	}
	return filtered, nil
}

func (c *Client) fetchArchiveSearch(ctx context.Context, query string) ([]Record, error) {
	tokens := searchTokens(query)
	if len(tokens) == 0 {
		return []Record{}, nil
	}
	meaningful := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if _, stop := titleSearchStopWords[token]; !stop {
			meaningful = append(meaningful, token)
		}
	}
	if len(meaningful) == 0 {
		meaningful = tokens
	}
	// Tokens are created only from Unicode letters and digits, so user input
	// cannot add Lucene operators or alter the collection/media filters.
	terms := strings.Join(meaningful, " AND ")
	queryValue := "collection:librivoxaudio AND mediatype:audio AND title:(" + terms + ")"
	cacheKey := strings.ToLower(queryValue)

	c.archiveMu.Lock()
	defer c.archiveMu.Unlock()
	if c.archiveQueries == nil {
		c.archiveQueries = make(map[string]cacheEntry)
	}
	now := time.Now()
	for key, entry := range c.archiveQueries {
		if !now.Before(entry.expires) {
			delete(c.archiveQueries, key)
		}
	}
	if cached, ok := c.archiveQueries[cacheKey]; ok && now.Before(cached.expires) {
		return cloneRecords(cached.records), nil
	}
	if len(c.archiveQueries) >= maxArchiveQueryCache {
		var oldestKey string
		oldestExpiry := time.Time{}
		for key, entry := range c.archiveQueries {
			if oldestKey == "" || entry.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = key, entry.expires
			}
		}
		delete(c.archiveQueries, oldestKey)
	}

	base, err := archiveBase(c.ArchiveBaseURL)
	if err != nil {
		return nil, err
	}
	params := url.Values{
		"q":      []string{queryValue},
		"rows":   []string{"20"},
		"page":   []string{"1"},
		"output": []string{"json"},
	}
	for _, field := range []string{
		"identifier", "title", "creator", "source", "description",
		"language", "runtime", "date",
	} {
		params.Add("fl[]", field)
	}
	endpoint := *base
	endpoint.Path = strings.TrimRight(base.Path, "/") + "/advancedsearch.php"
	endpoint.RawQuery = params.Encode()
	body, status, err := c.archiveGETLocked(ctx, &endpoint, maxArchiveResponse)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Internet Archive search returned HTTP %d", status)
	}
	var response archiveSearchResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("Internet Archive search response was invalid")
	}
	records := make([]Record, 0, len(response.Response.Docs))
	for _, doc := range response.Response.Docs {
		record, ok := archiveRecordFromSearchDoc(doc)
		if !ok || !archiveTitleContainsQueryTerms(record.Title, query) {
			continue
		}
		records = append(records, record)
	}
	c.archiveQueries[cacheKey] = cacheEntry{
		expires: time.Now().Add(archiveCacheDuration),
		records: cloneRecords(records),
	}
	return records, nil
}

func archiveTitleContainsQueryTerms(title, query string) bool {
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
	return required > 0
}

func (c *Client) archivePairs(ctx context.Context, records []Record) ([]Pair, error) {
	if len(records) == 0 {
		return []Pair{}, nil
	}
	index, err := c.projectGutenbergIndex(ctx)
	if err != nil {
		return nil, err
	}
	pairs := make([]Pair, 0, len(records))
	for _, record := range records {
		record.TextCandidates = textCandidatesForArchiveRecord(record, index)
		if pair, ok := ToPair(record); ok {
			pairs = append(pairs, pair)
		}
	}
	return pairs, nil
}

func archiveRecordFromSearchDoc(doc archiveSearchDoc) (Record, bool) {
	identifier := strings.ToLower(strings.TrimSpace(doc.Identifier))
	title := strings.TrimSpace(doc.Title)
	if !validArchiveIdentifier(identifier) || title == "" || len(title) > 300 || hasControls(title) {
		return Record{}, false
	}
	creator := doc.Creator.first()
	if len(creator) > 200 || hasControls(creator) {
		creator = ""
	}
	source := strings.Join(doc.Source, "\n")
	description := strings.Join(doc.Description, "\n")
	authorList := []Author{}
	if creator != "" {
		authorList = append(authorList, Author{FirstName: creator})
	}
	return Record{
		ID:                   archiveRecordPrefix + identifier,
		Title:                title,
		Language:             normalizeArchiveLanguage(doc.Language.first()),
		TotalTimeSecs:        parseArchiveDuration(doc.Runtime.first()),
		Authors:              authorList,
		Provider:             "internet_archive",
		ArchiveID:            identifier,
		ArchiveSource:        source,
		ArchiveDesc:          description,
		ArchiveGutenbergRefs: len(linkedGutenbergIDs(source, description)) > 0,
		ArchiveAudioURL:      "https://archive.org/details/" + identifier,
		Narrator:             archiveNarrator(description),
		URLZipFile:           archiveMP3ZipURL(identifier),
	}, true
}

func (c *Client) archiveByID(ctx context.Context, identifier string) (Record, error) {
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	if !validArchiveIdentifier(identifier) {
		return Record{}, fmt.Errorf("invalid Internet Archive item ID")
	}
	base, err := archiveBase(c.ArchiveBaseURL)
	if err != nil {
		return Record{}, err
	}
	endpoint := *base
	endpoint.Path = strings.TrimRight(base.Path, "/") + "/metadata/" + identifier
	body, status, err := c.archiveGET(ctx, &endpoint, maxArchiveResponse)
	if err != nil {
		return Record{}, err
	}
	if status != http.StatusOK {
		return Record{}, fmt.Errorf("Internet Archive item metadata returned HTTP %d", status)
	}
	var response archiveMetadataResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return Record{}, fmt.Errorf("Internet Archive item metadata was invalid")
	}
	meta := response.Metadata
	if strings.ToLower(meta.Identifier) != identifier ||
		!containsString(meta.Collection, "librivoxaudio") ||
		!strings.EqualFold(meta.MediaType.first(), "audio") ||
		strings.TrimSpace(meta.Title) == "" || !hasPublicMP3(response.Files) {
		return Record{}, fmt.Errorf("Internet Archive item is not a downloadable LibriVox audiobook")
	}
	record, ok := archiveRecordFromSearchDoc(archiveSearchDoc{
		Identifier: identifier, Title: meta.Title, Creator: meta.Creator, Source: meta.Source,
		Description: meta.Description, Language: meta.Language, Runtime: meta.Runtime, Date: meta.Date,
	})
	if !ok {
		return Record{}, fmt.Errorf("Internet Archive item metadata was invalid")
	}
	index, err := c.projectGutenbergIndex(ctx)
	if err != nil {
		return Record{}, err
	}
	record.TextCandidates = textCandidatesForArchiveRecord(record, index)
	if len(record.TextCandidates) == 0 {
		return Record{}, fmt.Errorf("no supported Gutenberg text candidate remains for this audio item")
	}
	return record, nil
}

func hasPublicMP3(files []archiveFile) bool {
	for _, file := range files {
		if isPrivateArchiveFile(file.Private) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(file.Name[strings.LastIndex(file.Name, ".")+1:]), "mp3") ||
			strings.Contains(strings.ToLower(strings.Join(file.Format, " ")), "mp3") {
			return true
		}
	}
	return false
}

func isPrivateArchiveFile(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var flag bool
	if json.Unmarshal(raw, &flag) == nil {
		return flag
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.EqualFold(text, "true") || text == "1"
	}
	return true
}

func archiveMP3ZipURL(identifier string) string {
	return "https://archive.org/compress/" + identifier + "/formats=64KBPS%20MP3"
}

func validArchiveIdentifier(identifier string) bool {
	if identifier == "" || len(identifier) > 100 || identifier != strings.ToLower(identifier) {
		return false
	}
	for _, r := range identifier {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return identifier[0] != '.' && identifier[0] != '-'
}

func archiveBase(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		raw = defaultArchiveBase
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("Internet Archive endpoint is invalid")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "https" && (host == "archive.org" || host == "www.archive.org") {
		return u, nil
	}
	ip := net.ParseIP(host)
	if u.Scheme == "http" && (host == "localhost" || (ip != nil && ip.IsLoopback())) {
		return u, nil
	}
	return nil, fmt.Errorf("Internet Archive endpoint is invalid")
}

func (c *Client) archiveGET(ctx context.Context, endpoint *url.URL, maxBytes int64) ([]byte, int, error) {
	c.archiveMu.Lock()
	defer c.archiveMu.Unlock()
	return c.archiveGETLocked(ctx, endpoint, maxBytes)
}

func (c *Client) archiveGETLocked(ctx context.Context, endpoint *url.URL, maxBytes int64) ([]byte, int, error) {
	wait := c.ArchiveRequestGap - time.Since(c.archiveLastRequest)
	if wait > 0 && !c.archiveLastRequest.IsZero() {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-timer.C:
		}
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 25 * time.Second}
	}
	safeClient := *client
	safeClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 || !sameArchiveOrigin(endpoint, req.URL) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("could not create Internet Archive request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Readalong/1.0 (LibriVox catalog search)")
	c.archiveLastRequest = time.Now()
	resp, err := safeClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("Internet Archive catalog is temporarily unavailable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil || int64(len(body)) > maxBytes {
		return nil, 0, fmt.Errorf("Internet Archive response could not be read")
	}
	return body, resp.StatusCode, nil
}

func sameArchiveOrigin(base, target *url.URL) bool {
	if base.Scheme != target.Scheme {
		return false
	}
	baseHost, targetHost := strings.ToLower(base.Hostname()), strings.ToLower(target.Hostname())
	if baseHost == "archive.org" || baseHost == "www.archive.org" {
		if targetHost != "archive.org" && targetHost != "www.archive.org" {
			return false
		}
	} else if baseHost != targetHost {
		return false
	}
	return effectivePort(base) == effectivePort(target)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), wanted) {
			return true
		}
	}
	return false
}

func parseArchiveDuration(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds >= 0 {
		return int64(seconds)
	}
	parts := strings.Split(raw, ":")
	if len(parts) == 3 {
		h, errH := strconv.ParseInt(parts[0], 10, 64)
		m, errM := strconv.ParseInt(parts[1], 10, 64)
		s, errS := strconv.ParseInt(parts[2], 10, 64)
		if errH != nil || errM != nil || errS != nil || h < 0 || m < 0 || m >= 60 || s < 0 || s >= 60 {
			return 0
		}
		return h*3600 + m*60 + s
	}
	if len(parts) != 2 {
		return 0
	}
	if dot := strings.IndexByte(parts[1], '.'); dot >= 0 {
		// Internet Archive uses H:MM.SS for some long items, while short
		// recordings commonly use MM:SS.
		h, errH := strconv.ParseInt(parts[0], 10, 64)
		m, errM := strconv.ParseInt(parts[1][:dot], 10, 64)
		s, errS := strconv.ParseInt(parts[1][dot+1:], 10, 64)
		if errH != nil || errM != nil || errS != nil || h < 0 || m < 0 || m >= 60 || s < 0 || s >= 60 {
			return 0
		}
		return h*3600 + m*60 + s
	}
	m, errM := strconv.ParseInt(parts[0], 10, 64)
	s, errS := strconv.ParseInt(parts[1], 10, 64)
	if errM != nil || errS != nil || m < 0 || s < 0 || s >= 60 {
		return 0
	}
	return m*60 + s
}

func normalizeArchiveLanguage(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "eng", "english":
		return "en"
	case "fre", "fra", "french":
		return "fr"
	case "ger", "deu", "german":
		return "de"
	case "spa", "spanish":
		return "es"
	case "ita", "italian":
		return "it"
	case "por", "portuguese":
		return "pt"
	default:
		return strings.ToLower(strings.TrimSpace(language))
	}
}

func (c *Client) projectGutenbergIndex(ctx context.Context) (*gutenbergIndex, error) {
	c.gutenbergMu.Lock()
	defer c.gutenbergMu.Unlock()
	now := time.Now()
	if c.gutenbergCache != nil && now.Sub(c.gutenbergCache.fetchedAt) < gutenbergCacheTTL {
		return c.gutenbergCache, nil
	}
	var stale *gutenbergIndex
	if c.gutenbergCache != nil {
		stale = c.gutenbergCache
	}
	if diskIndex, fresh, err := c.readGutenbergIndexCache(); err == nil && diskIndex != nil {
		stale = diskIndex
		c.gutenbergCache = diskIndex
		if fresh {
			return diskIndex, nil
		}
	}
	if now.Before(c.gutenbergRetryAt) {
		if stale != nil {
			return stale, nil
		}
		return nil, fmt.Errorf("Project Gutenberg catalog is temporarily unavailable")
	}
	compressed, err := c.downloadGutenbergCatalog(ctx)
	if err != nil {
		c.gutenbergRetryAt = now.Add(10 * time.Minute)
		if stale != nil {
			return stale, nil
		}
		return nil, err
	}
	index, err := parseGutenbergCatalog(compressed, time.Now())
	if err != nil {
		c.gutenbergRetryAt = now.Add(10 * time.Minute)
		if stale != nil {
			return stale, nil
		}
		return nil, err
	}
	c.writeGutenbergIndexCache(compressed)
	c.gutenbergRetryAt = time.Time{}
	c.gutenbergCache = index
	return index, nil
}

func (c *Client) downloadGutenbergCatalog(ctx context.Context) ([]byte, error) {
	rawURL := c.GutenbergCatalogURL
	if strings.TrimSpace(rawURL) == "" {
		rawURL = defaultGutenbergCatalog
	}
	endpoint, err := url.Parse(rawURL)
	if err != nil || !validGutenbergCatalogURL(endpoint) {
		return nil, fmt.Errorf("Project Gutenberg catalog endpoint is invalid")
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	safeClient := *client
	safeClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 || !sameGutenbergCatalogOrigin(endpoint, req.URL) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("could not create Project Gutenberg catalog request")
	}
	req.Header.Set("Accept", "application/gzip, application/octet-stream")
	req.Header.Set("User-Agent", "Readalong/1.0 (paired book catalog)")
	resp, err := safeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Project Gutenberg catalog is temporarily unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Project Gutenberg catalog returned HTTP %d", resp.StatusCode)
	}
	compressed, err := io.ReadAll(io.LimitReader(resp.Body, maxPGCatalogGzip+1))
	if err != nil || len(compressed) > maxPGCatalogGzip {
		return nil, fmt.Errorf("Project Gutenberg catalog download is too large")
	}
	return compressed, nil
}

func parseGutenbergCatalog(compressed []byte, fetchedAt time.Time) (*gutenbergIndex, error) {
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("Project Gutenberg catalog was invalid")
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: gz, N: maxPGCatalogCSV + 1}
	reader := csv.NewReader(limited)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("Project Gutenberg catalog was invalid")
	}
	columns := make(map[string]int, len(header))
	for i, name := range header {
		name = strings.TrimPrefix(strings.TrimSpace(name), "\ufeff")
		columns[name] = i
	}
	for _, name := range []string{"Text#", "Type", "Title", "Language", "Authors", "Issued"} {
		if _, ok := columns[name]; !ok {
			return nil, fmt.Errorf("Project Gutenberg catalog has an unsupported format")
		}
	}
	index := &gutenbergIndex{
		fetchedAt: fetchedAt,
		byID:      make(map[string]gutenbergEntry),
	}
	for count := 0; ; count++ {
		if count >= maxPGCatalogRows {
			return nil, fmt.Errorf("Project Gutenberg catalog contains too many records")
		}
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Project Gutenberg catalog could not be parsed")
		}
		get := func(name string) string {
			at := columns[name]
			if at >= len(row) {
				return ""
			}
			return strings.TrimSpace(row[at])
		}
		entry := gutenbergEntry{
			ID: get("Text#"), Type: get("Type"), Title: get("Title"), Language: normalizeArchiveLanguage(get("Language")),
			Authors: get("Authors"), Issued: get("Issued"),
		}
		if !digitsOnly(entry.ID) || entry.Type != "Text" || entry.Title == "" {
			continue
		}
		index.byID[entry.ID] = entry
		index.entries = append(index.entries, entry)
	}
	if len(index.entries) == 0 {
		return nil, fmt.Errorf("Project Gutenberg catalog contains no usable entries")
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("Project Gutenberg catalog expands beyond the supported size")
	}
	return index, nil
}

func (c *Client) gutenbergIndexCachePath() string {
	if strings.TrimSpace(c.CatalogCacheDir) == "" {
		return ""
	}
	return filepath.Join(c.CatalogCacheDir, "pg_catalog.csv.gz")
}

func (c *Client) readGutenbergIndexCache() (*gutenbergIndex, bool, error) {
	path := c.gutenbergIndexCachePath()
	if path == "" {
		return nil, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("invalid cached Project Gutenberg catalog")
	}
	compressed, err := io.ReadAll(io.LimitReader(file, maxPGCatalogGzip+1))
	if err != nil || len(compressed) > maxPGCatalogGzip {
		return nil, false, fmt.Errorf("cached Project Gutenberg catalog is too large")
	}
	index, err := parseGutenbergCatalog(compressed, info.ModTime())
	if err != nil {
		return nil, false, err
	}
	return index, time.Since(info.ModTime()) < gutenbergCacheTTL, nil
}

func (c *Client) writeGutenbergIndexCache(compressed []byte) {
	path := c.gutenbergIndexCachePath()
	if path == "" || len(compressed) == 0 || len(compressed) > maxPGCatalogGzip {
		return
	}
	dir := filepath.Dir(path)
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	file, err := os.CreateTemp(dir, ".pg-catalog-*.partial")
	if err != nil {
		return
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if file.Chmod(0600) != nil {
		_ = file.Close()
		return
	}
	if _, err := file.Write(compressed); err != nil {
		_ = file.Close()
		return
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func validGutenbergCatalogURL(u *url.URL) bool {
	if u == nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "https" && (host == "www.gutenberg.org" || host == "gutenberg.org") {
		return true
	}
	ip := net.ParseIP(host)
	return u.Scheme == "http" && (host == "localhost" || (ip != nil && ip.IsLoopback()))
}

func sameGutenbergCatalogOrigin(base, target *url.URL) bool {
	if base.Scheme != target.Scheme {
		return false
	}
	baseHost, targetHost := strings.ToLower(base.Hostname()), strings.ToLower(target.Hostname())
	if baseHost == "www.gutenberg.org" || baseHost == "gutenberg.org" {
		if targetHost != "www.gutenberg.org" && targetHost != "gutenberg.org" {
			return false
		}
	} else if baseHost != targetHost {
		return false
	}
	return effectivePort(base) == effectivePort(target)
}

func textCandidatesForArchiveRecord(record Record, index *gutenbergIndex) []TextCandidate {
	if index == nil {
		return nil
	}
	linkedIDs := linkedGutenbergIDs(record.ArchiveSource, record.ArchiveDesc)
	linked := make([]TextCandidate, 0, len(linkedIDs))
	for _, id := range linkedIDs {
		entry, ok := index.byID[id]
		if !ok || !archiveLinkMatchesText(record, entry) {
			continue
		}
		if candidate, ok := makeTextCandidate(entry, "source_linked"); ok {
			linked = append(linked, candidate)
		}
	}
	if len(linked) > 0 {
		return dedupeTextCandidates(linked)
	}
	return catalogTextCandidates(record, index)
}

func linkedGutenbergIDs(source, description string) []string {
	text := html.UnescapeString(source + "\n" + description)
	seen := make(map[string]bool)
	var ids []string
	for _, match := range gutenbergURLPattern.FindAllStringSubmatch(text, -1) {
		if len(match) == 2 && !seen[match[1]] {
			seen[match[1]] = true
			ids = append(ids, match[1])
		}
	}
	for _, match := range gutenbergTextPattern.FindAllStringSubmatch(text, -1) {
		if len(match) == 2 && !seen[match[1]] {
			seen[match[1]] = true
			ids = append(ids, match[1])
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, _ := strconv.ParseUint(ids[i], 10, 64)
		b, _ := strconv.ParseUint(ids[j], 10, 64)
		return a < b
	})
	return ids
}

func archiveLinkMatchesText(record Record, entry gutenbergEntry) bool {
	audioTitle := titleWords(record.Title)
	bookTitle := titleWords(entry.Title)
	if len(audioTitle) == 0 || len(bookTitle) == 0 {
		return false
	}
	intersection := 0
	for word := range audioTitle {
		if bookTitle[word] {
			intersection++
		}
	}
	smaller := min(len(audioTitle), len(bookTitle))
	if smaller == 0 || intersection < min(2, smaller) || float64(intersection)/float64(smaller) < 0.70 {
		return false
	}
	archiveAuthor := strings.Join(recordAuthorNames(record), " ")
	if archiveAuthor != "" && entry.Authors != "" && !authorNamesMatch(archiveAuthor, entry.Authors) {
		return false
	}
	if record.Language != "" && entry.Language != "" && normalizeArchiveLanguage(record.Language) != entry.Language {
		return false
	}
	return true
}

func catalogTextCandidates(record Record, index *gutenbergIndex) []TextCandidate {
	audioTitle := cleanMatchTitle(record.Title)
	author := strings.Join(recordAuthorNames(record), " ")
	if len(audioTitle) == 0 || strings.TrimSpace(author) == "" {
		return nil
	}
	var candidates []TextCandidate
	for _, entry := range index.entries {
		if !equalTokens(cleanMatchTitle(entry.Title), audioTitle) ||
			!authorNamesMatch(author, entry.Authors) ||
			(record.Language != "" && entry.Language != "" && normalizeArchiveLanguage(record.Language) != entry.Language) {
			continue
		}
		if candidate, ok := makeTextCandidate(entry, "title_author"); ok {
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, _ := strconv.ParseUint(candidates[i].GutenbergID, 10, 64)
		b, _ := strconv.ParseUint(candidates[j].GutenbergID, 10, 64)
		return a < b
	})
	if len(candidates) > 8 {
		candidates = candidates[:8]
	}
	return dedupeTextCandidates(candidates)
}

func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func makeTextCandidate(entry gutenbergEntry, basis string) (TextCandidate, bool) {
	if len(entry.Title) > 300 || hasControls(entry.Title) || len(entry.Authors) > 400 {
		return TextCandidate{}, false
	}
	link, err := GutenbergURL(entry.ID)
	if err != nil {
		return TextCandidate{}, false
	}
	return TextCandidate{
		GutenbergID: entry.ID, Title: entry.Title, Author: displayPGAuthors(entry.Authors),
		Language: entry.Language, Issued: entry.Issued, GutenbergURL: link, MatchBasis: basis,
	}, true
}

func dedupeTextCandidates(candidates []TextCandidate) []TextCandidate {
	seen := make(map[string]bool, len(candidates))
	result := make([]TextCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.GutenbergID == "" || seen[candidate.GutenbergID] {
			continue
		}
		seen[candidate.GutenbergID] = true
		result = append(result, candidate)
	}
	return result
}

func recordAuthorNames(record Record) []string {
	var names []string
	for _, author := range record.Authors {
		name := strings.TrimSpace(strings.Join([]string{author.FirstName, author.LastName}, " "))
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func authorNamesMatch(archiveAuthor, gutenbergAuthors string) bool {
	a := nameWords(archiveAuthor)
	g := nameWords(gutenbergAuthors)
	if len(a) == 0 || len(g) == 0 {
		return false
	}
	overlap := 0
	for word := range a {
		if g[word] {
			overlap++
		}
	}
	return overlap >= min(2, len(a)) && float64(overlap)/float64(len(a)) >= 0.60
}

func nameWords(value string) map[string]bool {
	words := make(map[string]bool)
	var builder strings.Builder
	flush := func() {
		word := builder.String()
		builder.Reset()
		if word == "" || len([]rune(word)) == 1 && word[0] >= '0' && word[0] <= '9' {
			return
		}
		if _, stop := map[string]struct{}{
			"illustrator": {}, "editor": {}, "translator": {}, "author": {},
		}[word]; stop {
			return
		}
		allDigits := true
		for _, r := range word {
			if !unicode.IsDigit(r) {
				allDigits = false
				break
			}
		}
		if !allDigits {
			words[word] = true
		}
	}
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	for word := range words {
		if len(word) == 4 {
			if _, err := strconv.Atoi(word); err == nil {
				delete(words, word)
			}
		}
	}
	return words
}

func titleWords(value string) map[string]bool {
	words := make(map[string]bool)
	for _, token := range searchTokens(cleanMatchTitleString(value)) {
		if _, stop := titleSearchStopWords[token]; !stop {
			allDigits := true
			for _, r := range token {
				if !unicode.IsDigit(r) {
					allDigits = false
					break
				}
			}
			if !allDigits {
				words[token] = true
			}
		}
	}
	return words
}

func cleanMatchTitle(value string) []string {
	return searchTokens(cleanMatchTitleString(value))
}

func cleanMatchTitleString(value string) string {
	value = strings.TrimSpace(value)
	value = bylineSuffixPattern.ReplaceAllString(value, "")
	value = versionSuffixPattern.ReplaceAllString(value, "")
	value = dramaSuffixPattern.ReplaceAllString(value, "")
	return strings.TrimSpace(value)
}

func displayPGAuthors(value string) string {
	authors := strings.Split(value, ";")
	if len(authors) == 0 {
		return strings.TrimSpace(value)
	}
	first := strings.TrimSpace(authors[0])
	if paren := strings.Index(first, "("); paren >= 0 {
		if end := strings.Index(first[paren+1:], ")"); end >= 0 {
			full := strings.TrimSpace(first[paren+1 : paren+1+end])
			if full != "" {
				family := strings.TrimSpace(strings.SplitN(first[:paren], ",", 2)[0])
				return strings.TrimSpace(full + " " + family)
			}
		}
	}
	first = regexp.MustCompile(`,\s*\d{3,4}.*$`).ReplaceAllString(first, "")
	first = regexp.MustCompile(`\s*\[[^]]+\]`).ReplaceAllString(first, "")
	parts := strings.SplitN(first, ",", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[1] + " " + parts[0])
	}
	return first
}

func makeArchivePair(record Record) (Pair, bool) {
	if record.Provider != "internet_archive" || !validArchiveIdentifier(record.ArchiveID) ||
		record.Title == "" || !safeArchiveURL(record.URLZipFile) ||
		len(record.TextCandidates) == 0 {
		return Pair{}, false
	}
	provider := "internet_archive"
	matchKind := record.TextCandidates[0].MatchBasis
	matchNote := "A Gutenberg ID is linked in the IA metadata. Edition match is not verified."
	if matchKind != "source_linked" {
		matchKind = "title_author"
		if record.ArchiveGutenbergRefs {
			matchNote = "IA mentioned a Gutenberg text that did not fit this recording. Suggestions match only title, creator, and language; check the selected edition."
		} else {
			matchNote = "No Gutenberg ID was present in the audio metadata. Suggestions match title, creator, and language; verify the selected edition."
		}
	}
	pair := Pair{
		RecordID: record.ID, Provider: provider, Title: record.Title,
		Authors: recordAuthorNames(record), Narrator: record.Narrator, Language: record.Language,
		DurationMS: record.TotalTimeSecs * 1000, AudioSourceURL: record.ArchiveAudioURL,
		LibriVoxURL: archiveLibriVoxPageURL(record.ArchiveDesc),
		MatchKind:   matchKind, MatchNote: matchNote, TextCandidates: append([]TextCandidate(nil), record.TextCandidates...),
	}
	if len(record.TextCandidates) == 1 {
		pair.GutenbergID = record.TextCandidates[0].GutenbergID
		pair.GutenbergURL = record.TextCandidates[0].GutenbergURL
	}
	return pair, true
}

func archiveLibriVoxPageURL(description string) string {
	for _, raw := range lvPageURLPattern.FindAllString(html.UnescapeString(description), -1) {
		raw = strings.TrimRight(raw, ".,;)")
		u, err := url.Parse(raw)
		if err != nil || u.User != nil ||
			(u.Scheme != "https" && u.Scheme != "http") ||
			(strings.ToLower(u.Hostname()) != "librivox.org" && strings.ToLower(u.Hostname()) != "www.librivox.org") {
			continue
		}
		path := strings.Trim(u.EscapedPath(), "/")
		if path == "" || strings.HasPrefix(strings.ToLower(path), "rss/") {
			continue
		}
		return "https://librivox.org/" + path
	}
	return ""
}

func archiveNarrator(description string) string {
	text := strings.TrimSpace(htmlTagPattern.ReplaceAllString(html.UnescapeString(description), " "))
	text = strings.Join(strings.Fields(text), " ")
	match := narratorPattern.FindStringSubmatch(text)
	if len(match) != 2 {
		return ""
	}
	narrator := strings.Trim(strings.TrimSpace(match[1]), " ,:-")
	if len(narrator) > 160 {
		return ""
	}
	return narrator
}

// SelectTextCandidate binds the user's choice to the candidates recomputed
// from the freshly fetched source record; client-supplied Gutenberg IDs alone
// are never accepted.
func SelectTextCandidate(pair Pair, id string) (Pair, bool) {
	if pair.Provider != "internet_archive" {
		if id != "" && id != pair.GutenbergID {
			return Pair{}, false
		}
		return pair, pair.GutenbergID != ""
	}
	for _, candidate := range pair.TextCandidates {
		if candidate.GutenbergID == id {
			pair.GutenbergID = candidate.GutenbergID
			pair.GutenbergURL = candidate.GutenbergURL
			pair.MatchKind = candidate.MatchBasis
			if pair.MatchKind == "source_linked" {
				pair.MatchNote = "A Gutenberg ID is linked in the IA metadata. Edition match is not verified."
			} else if pair.MatchNote == "" {
				pair.MatchNote = "No Gutenberg ID was present in the audio metadata. Suggestions match title, creator, and language; verify the selected edition."
			}
			return pair, true
		}
	}
	return Pair{}, false
}

func cloneRecords(records []Record) []Record {
	clone := append([]Record(nil), records...)
	for i := range clone {
		clone[i].Authors = append([]Author(nil), records[i].Authors...)
		clone[i].TextCandidates = append([]TextCandidate(nil), records[i].TextCandidates...)
	}
	return clone
}
