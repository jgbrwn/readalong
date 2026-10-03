package covers

import (
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
	"sync"
	"time"
	"unicode"
)

const (
	defaultOpenLibrarySearch = "https://openlibrary.org/search.json"
	openLibraryRequestGap    = time.Second
	maxSearchResponseBytes   = 2 << 20
)

type Candidate struct {
	ImageURL      string
	SourceURL     string
	Year          int
	TitleExact    bool
	AuthorOverlap float64
}

type OpenLibrary struct {
	BaseURL string
	Contact string
	HTTP    *http.Client

	mu          sync.Mutex
	lastRequest time.Time
}

type searchResponse struct {
	Docs []searchDoc `json:"docs"`
}

type searchDoc struct {
	Key              string   `json:"key"`
	Title            string   `json:"title"`
	Authors          []string `json:"author_name"`
	FirstPublishYear int      `json:"first_publish_year"`
	CoverID          int      `json:"cover_i"`
}

var openLibraryRecordPattern = regexp.MustCompile(`^/(?:works/OL[0-9]+W|books/OL[0-9]+M)$`)

func (c *OpenLibrary) SearchCover(ctx context.Context, title, author string) (Candidate, bool, error) {
	title = strings.TrimSpace(title)
	author = strings.TrimSpace(author)
	if title == "" || len(title) > 255 || len(author) > 255 || hasControls(title) || hasControls(author) {
		return Candidate{}, false, fmt.Errorf("book metadata is unavailable")
	}
	endpoint, err := c.searchURL()
	if err != nil {
		return Candidate{}, false, err
	}
	query := endpoint.Query()
	query.Set("title", title)
	if author != "" {
		query.Set("author", author)
	}
	query.Set("fields", "key,title,author_name,first_publish_year,cover_i")
	query.Set("limit", "20")
	endpoint.RawQuery = query.Encode()

	body, status, err := c.get(ctx, endpoint)
	if err != nil {
		return Candidate{}, false, err
	}
	if status != http.StatusOK {
		return Candidate{}, false, fmt.Errorf("Open Library search returned HTTP %d", status)
	}
	var response searchResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return Candidate{}, false, fmt.Errorf("Open Library search response was invalid")
	}
	if len(response.Docs) > 100 {
		return Candidate{}, false, fmt.Errorf("Open Library search response was too large")
	}

	titleTokens := normalizedTokens(title)
	if len(titleTokens) == 0 {
		return Candidate{}, false, fmt.Errorf("book title is not searchable")
	}
	type scoredCandidate struct {
		value Candidate
		score int
	}
	var candidates []scoredCandidate
	for _, doc := range response.Docs {
		if !openLibraryRecordPattern.MatchString(doc.Key) {
			continue
		}
		docTitle := normalizedTokens(doc.Title)
		overlap := tokenOverlap(titleTokens, docTitle)
		titleExact := overlap == 1 && equalTokens(titleTokens, docTitle)
		if overlap < 0.75 {
			continue
		}
		authorOverlap := 0.0
		if author != "" && len(doc.Authors) > 0 {
			authorOverlap = maxTokenOverlap(normalizedTokens(author), normalizedTokens(strings.Join(doc.Authors, " ")))
		}
		if author != "" && authorOverlap == 0 && len(doc.Authors) > 0 {
			continue
		}
		year := doc.FirstPublishYear
		if !titleExact || author == "" || authorOverlap < 0.75 || year < 1000 || year > time.Now().Year()+2 {
			year = 0
		}
		sourceURL := "https://openlibrary.org" + doc.Key
		imageURL := ""
		if doc.CoverID > 0 && doc.CoverID <= 1_000_000_000 {
			imageURL = "https://covers.openlibrary.org/b/id/" + strconv.Itoa(doc.CoverID) + "-M.jpg?default=false"
		}
		score := int(overlap*60) + int(authorOverlap*40)
		if titleExact {
			score += 20
		}
		candidates = append(candidates, scoredCandidate{
			value: Candidate{ImageURL: imageURL, SourceURL: sourceURL, Year: year,
				TitleExact: titleExact, AuthorOverlap: authorOverlap},
			score: score,
		})
	}
	if len(candidates) == 0 {
		return Candidate{}, false, nil
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	return candidates[0].value, candidates[0].value.ImageURL != "", nil
}

func (c *OpenLibrary) searchURL() (*url.URL, error) {
	raw := strings.TrimSpace(c.BaseURL)
	if raw == "" {
		raw = defaultOpenLibrarySearch
	}
	u, err := url.Parse(raw)
	if err != nil || !validOpenLibraryURL(u) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("Open Library search endpoint is invalid")
	}
	return u, nil
}

func (c *OpenLibrary) get(ctx context.Context, endpoint *url.URL) ([]byte, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := openLibraryRequestGap - time.Since(c.lastRequest); wait > 0 && !c.lastRequest.IsZero() {
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
		client = &http.Client{Timeout: 8 * time.Second}
	}
	safeClient := *client
	host := strings.ToLower(endpoint.Hostname())
	safeClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 2 || !validOpenLibraryURL(req.URL) ||
			!strings.EqualFold(req.URL.Hostname(), host) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	agent := "Readalong/1.0 cover discovery"
	if contact := strings.TrimSpace(c.Contact); contact != "" && len(contact) < 200 && !hasControls(contact) {
		agent += " (" + contact + ")"
	}
	request.Header.Set("User-Agent", agent)
	request.Header.Set("Accept", "application/json")
	c.lastRequest = time.Now()
	response, err := safeClient.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxSearchResponseBytes+1))
	if err != nil || len(body) > maxSearchResponseBytes {
		return nil, response.StatusCode, fmt.Errorf("Open Library response could not be read")
	}
	return body, response.StatusCode, nil
}

func validOpenLibraryURL(u *url.URL) bool {
	if u == nil || u.Host == "" || u.User != nil {
		return false
	}
	if u.Scheme == "https" && strings.EqualFold(u.Hostname(), "openlibrary.org") {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

func normalizedTokens(value string) []string {
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

func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, token := range a {
		counts[token]++
	}
	for _, token := range b {
		counts[token]--
		if counts[token] < 0 {
			return false
		}
	}
	return true
}

func tokenOverlap(wanted, found []string) float64 {
	if len(wanted) == 0 || len(found) == 0 {
		return 0
	}
	available := make(map[string]int, len(found))
	for _, token := range found {
		available[token]++
	}
	matched := 0
	for _, token := range wanted {
		if available[token] > 0 {
			matched++
			available[token]--
		}
	}
	return float64(matched) / float64(len(wanted))
}

func maxTokenOverlap(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	return max(tokenOverlap(a, b), tokenOverlap(b, a))
}

func hasControls(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
