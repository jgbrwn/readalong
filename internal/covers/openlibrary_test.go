package covers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestOpenLibraryChoosesExactTitleAuthorAndBuildsKnownURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search.json" {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		if query.Get("title") != "The Moonstone" || query.Get("author") != "Wilkie Collins" ||
			query.Get("fields") != "key,title,author_name,first_publish_year,cover_i" || query.Get("limit") != "20" {
			t.Errorf("unexpected Open Library query: %s", r.URL.RawQuery)
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("missing identifying User-Agent")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"docs": []any{
			map[string]any{
				"key": "/works/OL123W", "title": "The Moonstone", "author_name": []string{"Wilkie Collins"},
				"first_publish_year": 1868, "cover_i": 5678,
			},
			map[string]any{
				"key": "/works/OL124W", "title": "Moonstone Stories", "author_name": []string{"Other Author"},
				"first_publish_year": 2000, "cover_i": 9999,
			},
		}})
	}))
	defer server.Close()

	client := &OpenLibrary{BaseURL: server.URL + "/search.json", HTTP: server.Client()}
	candidate, found, err := client.SearchCover(context.Background(), "The Moonstone", "Wilkie Collins")
	if err != nil || !found {
		t.Fatalf("cover search found=%v err=%v", found, err)
	}
	if !candidate.TitleExact || candidate.AuthorOverlap != 1 || candidate.Year != 1868 ||
		candidate.ImageURL != "https://covers.openlibrary.org/b/id/5678-M.jpg?default=false" ||
		candidate.SourceURL != "https://openlibrary.org/works/OL123W" {
		t.Fatalf("unexpected candidate: %#v", candidate)
	}
	if _, err := url.Parse(candidate.ImageURL); err != nil {
		t.Fatal(err)
	}
}

func TestOpenLibraryDoesNotAutoMatchDifferentAuthorOrBadRecordKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"docs": []any{
			map[string]any{
				"key": "/works/OL1W", "title": "Middlemarch", "author_name": []string{"Different Author"}, "cover_i": 44,
			},
			map[string]any{
				"key": "/works/../unsafe", "title": "Middlemarch", "author_name": []string{"George Eliot"}, "cover_i": 45,
			},
		}})
	}))
	defer server.Close()
	client := &OpenLibrary{BaseURL: server.URL + "/search.json", HTTP: server.Client()}
	_, found, err := client.SearchCover(context.Background(), "Middlemarch", "George Eliot")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("unsafe or mismatched cover candidate accepted")
	}
}

func TestOpenLibraryTitleOnlyMatchRequiresReview(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"docs": []any{map[string]any{
			"key": "/books/OL99M", "title": "The Wind in the Willows",
			"author_name": []string{"Kenneth Grahame"}, "first_publish_year": 1908, "cover_i": 100,
		}}})
	}))
	defer server.Close()
	client := &OpenLibrary{BaseURL: server.URL + "/search.json", HTTP: server.Client()}
	candidate, found, err := client.SearchCover(context.Background(), "The Wind in the Willows", "")
	if err != nil || !found {
		t.Fatalf("candidate found=%v err=%v", found, err)
	}
	if !candidate.TitleExact || candidate.AuthorOverlap != 0 || candidate.Year != 0 {
		t.Fatalf("unexpected title-only candidate: %#v", candidate)
	}
}
