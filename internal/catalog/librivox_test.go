package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSearchReturnsOnlyLibriVoxGutenbergPairs(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("title") != "Anne of Green Gables" ||
			r.URL.Query().Get("format") != "json" ||
			r.URL.Query().Get("extended") != "1" || r.URL.Query().Get("limit") != "20" {
			t.Errorf("unexpected catalog query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(Response{Books: []Record{
			{
				ID: "146", Title: "Anne of Green Gables", Language: "English",
				URLTextSource: "https://www.gutenberg.org/etext/45",
				URLZipFile:    "https://archive.org/compress/anne_book/formats=64KBPS%20MP3%26file=%2Fbook.zip",
				URLLibriVox:   "https://librivox.org/anne/",
				TotalTimeSecs: 37811,
				Authors:       []Author{{FirstName: "Lucy Maud", LastName: "Montgomery"}},
			},
			{
				ID: "147", Title: "No paired text",
				URLTextSource: "https://example.org/text",
				URLZipFile:    "https://archive.org/download/book.zip",
				URLLibriVox:   "https://librivox.org/book/",
			},
		}})
	}))
	defer server.Close()

	client := NewClient()
	client.BaseURL, client.HTTP = server.URL, server.Client()
	pairs, err := client.Search(context.Background(), "Anne of Green Gables")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].RecordID != "146" || pairs[0].GutenbergID != "45" ||
		pairs[0].GutenbergURL != "https://www.gutenberg.org/ebooks/45" || pairs[0].DurationMS != 37811000 {
		t.Fatalf("unexpected pairs: %#v", pairs)
	}
	if calls != 1 {
		t.Fatalf("catalog calls = %d", calls)
	}
	if _, err := client.ByID(context.Background(), "146"); err != nil {
		t.Fatalf("cached catalog record should be reusable: %v", err)
	}
	if calls != 1 {
		t.Fatalf("record import triggered another query: %d calls", calls)
	}
}

func TestCatalogRejectsOffHostRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("off-host redirect was followed")
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()

	client := NewClient()
	client.BaseURL, client.HTTP = server.URL, server.Client()
	if _, err := client.Search(context.Background(), "some title"); err == nil {
		t.Fatal("expected redirect response to be rejected")
	}
}

func TestToPairRejectsUntrustedSources(t *testing.T) {
	record := Record{
		ID: "12", Title: "Example",
		URLTextSource: "https://www.gutenberg.org/ebooks/34",
		URLZipFile:    "https://archive.org/compress/example/formats=64KBPS%20MP3",
		URLLibriVox:   "https://librivox.org/example/",
	}
	if _, ok := ToPair(record); !ok {
		t.Fatal("valid source pair rejected")
	}
	record.URLZipFile = "https://archive.org.attacker.invalid/compress/example"
	if _, ok := ToPair(record); ok {
		t.Fatal("archive URL with a lookalike host accepted")
	}
	record.URLZipFile = "https://archive.org/compress/example"
	record.URLTextSource = "https://www.gutenberg.org.evil.invalid/ebooks/34"
	if _, ok := ToPair(record); ok {
		t.Fatal("Gutenberg URL with a lookalike host accepted")
	}
}

func TestGutenbergIDOnlyAcceptsCanonicalNumericPaths(t *testing.T) {
	for _, raw := range []string{"https://www.gutenberg.org/ebooks/1?format=html", "http://www.gutenberg.org/etext/3", "https://evil.invalid/ebooks/2"} {
		if _, ok := gutenbergID(raw); ok {
			t.Errorf("accepted non-canonical text source %q", raw)
		}
	}
	if id, ok := gutenbergID("https://www.gutenberg.org/etext/45"); !ok || id != "45" {
		t.Fatalf("ID = %q, ok=%v", id, ok)
	}
}

func TestSearchRequiresBoundedQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Response{})
	}))
	defer server.Close()
	client := NewClient()
	client.BaseURL, client.HTTP = server.URL, server.Client()
	if _, err := client.Search(context.Background(), "x"); err == nil {
		t.Fatal("one-character query accepted")
	}
	if _, err := client.Search(context.Background(), "a"+url.QueryEscape("b")); err != nil {
		t.Fatalf("valid query rejected: %v", err)
	}
}

func TestSearchTreatsDocumentedNoResults404AsEmptyAndCachesIt(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Audiobooks could not be found"}`))
	}))
	defer server.Close()
	client := NewClient()
	client.BaseURL, client.HTTP, client.minRequestGap = server.URL, server.Client(), 0
	for range 2 {
		pairs, err := client.Search(context.Background(), "Titus Groan")
		if err != nil {
			t.Fatalf("no-match response should not appear as an outage: %v", err)
		}
		if len(pairs) != 0 {
			t.Fatalf("unexpected pairs: %#v", pairs)
		}
	}
	if calls != 1 {
		t.Fatalf("cached no-match search made %d requests", calls)
	}
}

func TestSearchRetriesTransientCatalogFailureOnce(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(522)
			_, _ = w.Write([]byte("upstream timeout"))
			return
		}
		_ = json.NewEncoder(w).Encode(Response{Books: []Record{{
			ID: "391", Title: "Gift of the Magi",
			URLTextSource: "https://www.gutenberg.org/ebooks/7256",
			URLZipFile:    "https://archive.org/compress/gift-of-the-magi/formats=64KBPS%20MP3",
			URLLibriVox:   "https://librivox.org/gift-of-the-magi/",
		}}})
	}))
	defer server.Close()
	client := NewClient()
	client.BaseURL, client.HTTP, client.minRequestGap = server.URL, server.Client(), 0
	pairs, err := client.Search(context.Background(), "Gift of the Magi")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(pairs) != 1 || pairs[0].RecordID != "391" {
		t.Fatalf("calls=%d pairs=%#v", calls, pairs)
	}
}

func TestSearchRetriesRequestTimeoutOnce(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		_ = json.NewEncoder(w).Encode(Response{Books: []Record{{
			ID: "391", Title: "Gift of the Magi",
			URLTextSource: "https://www.gutenberg.org/ebooks/7256",
			URLZipFile:    "https://archive.org/compress/gift-of-the-magi/formats=64KBPS%20MP3",
			URLLibriVox:   "https://librivox.org/gift-of-the-magi/",
		}}})
	}))
	defer server.Close()
	client := NewClient()
	client.BaseURL, client.HTTP, client.minRequestGap = server.URL, server.Client(), 0
	pairs, err := client.Search(context.Background(), "Gift of the Magi")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(pairs) != 1 || pairs[0].RecordID != "391" {
		t.Fatalf("calls=%d pairs=%#v", calls, pairs)
	}
}

func TestSearchDoesNotRetryCatalogRateLimit(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := NewClient()
	client.BaseURL, client.HTTP, client.minRequestGap = server.URL, server.Client(), 0
	if _, err := client.Search(context.Background(), "Gift of the Magi"); err == nil {
		t.Fatal("expected rate-limit error")
	}
	if calls != 1 {
		t.Fatalf("rate limit response made %d requests, want 1", calls)
	}
}

func TestSearchRetriesCatalogRateLimitWithShortRetryAfter(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(Response{Books: []Record{{
			ID: "391", Title: "Gift of the Magi",
			URLTextSource: "https://www.gutenberg.org/ebooks/7256",
			URLZipFile:    "https://archive.org/compress/gift-of-the-magi/formats=64KBPS%20MP3",
			URLLibriVox:   "https://librivox.org/gift-of-the-magi/",
		}}})
	}))
	defer server.Close()
	client := NewClient()
	client.BaseURL, client.HTTP, client.minRequestGap = server.URL, server.Client(), 0
	pairs, err := client.Search(context.Background(), "Gift of the Magi")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(pairs) != 1 || pairs[0].RecordID != "391" {
		t.Fatalf("calls=%d pairs=%#v", calls, pairs)
	}
}

func TestSearchDoesNotRetryCatalogLongRetryAfter(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := NewClient()
	client.BaseURL, client.HTTP, client.minRequestGap = server.URL, server.Client(), 0
	if _, err := client.Search(context.Background(), "Gift of the Magi"); err == nil {
		t.Fatal("expected unavailable error")
	} else if !strings.Contains(err.Error(), "retry after 30s") {
		t.Fatalf("long Retry-After was not explained: %v", err)
	}
	if calls != 1 {
		t.Fatalf("long Retry-After made %d requests, want 1", calls)
	}
}

func TestSearchFallsBackWhenSmallTitleWordsAreMissing(t *testing.T) {
	var titles []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title := r.URL.Query().Get("title")
		titles = append(titles, title)
		if title == "Anne Green Gables" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Audiobooks could not be found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(Response{Books: []Record{{
			ID: "146", Title: "Anne of Green Gables",
			URLTextSource: "https://www.gutenberg.org/etext/45",
			URLZipFile:    "https://archive.org/compress/anne-book/formats=64KBPS%20MP3",
			URLLibriVox:   "https://librivox.org/anne/",
		}}})
	}))
	defer server.Close()
	client := NewClient()
	client.BaseURL, client.HTTP, client.minRequestGap = server.URL, server.Client(), 0
	pairs, err := client.Search(context.Background(), "Anne Green Gables")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].Title != "Anne of Green Gables" {
		t.Fatalf("omitted-word query returned %#v", pairs)
	}
	if len(titles) != 2 || titles[0] != "Anne Green Gables" || titles[1] != "green gables" {
		t.Fatalf("unexpected fallback queries: %#v", titles)
	}
}

func TestSearchFallbackRejectsTitlesMissingRequestedWords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("title") == "Anne Green Gables" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Audiobooks could not be found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(Response{Books: []Record{{
			ID: "146", Title: "Green Gables Stories",
			URLTextSource: "https://www.gutenberg.org/etext/45",
			URLZipFile:    "https://archive.org/compress/anne-book/formats=64KBPS%20MP3",
			URLLibriVox:   "https://librivox.org/anne/",
		}}})
	}))
	defer server.Close()
	client := NewClient()
	client.BaseURL, client.HTTP, client.minRequestGap = server.URL, server.Client(), 0
	pairs, err := client.Search(context.Background(), "Anne Green Gables")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 0 {
		t.Fatalf("fallback returned a title missing an input term: %#v", pairs)
	}
}
