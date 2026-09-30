package catalog

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func gzipPGCatalog(t *testing.T, rows string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write([]byte("Text#,Type,Issued,Title,Language,Authors,Subjects,LoCC,Bookshelves\n" + rows)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestArchiveSearchUsesExplicitGutenbergReference(t *testing.T) {
	pgCatalog := gzipPGCatalog(t, `7256,Text,2005-01-01,The Gift of the Magi,en,"Henry, O., 1862-1910",Stories,PS,Classics`+"\n")
	var searches, pgRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/advancedsearch.php":
			searches.Add(1)
			q := r.URL.Query().Get("q")
			if q != "collection:librivoxaudio AND mediatype:audio AND title:(gift AND magi)" {
				t.Errorf("unexpected Internet Archive query %q", q)
			}
			for _, field := range r.URL.Query()["fl[]"] {
				if field == "description" && r.URL.Query().Get("rows") == "20" {
					_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"docs": []any{map[string]any{
						"identifier": "giftofmagi", "title": "The Gift of the Magi", "creator": "O. Henry",
						"language": "eng", "runtime": "13:22", "source": "Librivox recording of Gutenberg e-text #7256",
						"licenseurl": "http://creativecommons.org/licenses/publicdomain/",
					}}}})
					return
				}
			}
			t.Error("search omitted the descriptive source fields")
		case "/pg_catalog.csv.gz":
			pgRequests.Add(1)
			_, _ = w.Write(pgCatalog)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.ArchiveEnabled = true
	client.ArchiveBaseURL = server.URL
	client.GutenbergCatalogURL = server.URL + "/pg_catalog.csv.gz"
	client.HTTP = server.Client()
	pairs, err := client.Search(context.Background(), "Gift of Magi")
	if err != nil {
		t.Fatal(err)
	}
	if searches.Load() != 1 || pgRequests.Load() != 1 || len(pairs) != 1 {
		t.Fatalf("searches=%d pgRequests=%d pairs=%#v", searches.Load(), pgRequests.Load(), pairs)
	}
	pair := pairs[0]
	if pair.Provider != "internet_archive" || pair.RecordID != "ia-giftofmagi" ||
		pair.MatchKind != "source_linked" || pair.GutenbergID != "7256" ||
		pair.GutenbergURL != "https://www.gutenberg.org/ebooks/7256" {
		t.Fatalf("unexpected source-linked result: %#v", pair)
	}
	if !strings.HasPrefix(pair.AudioSourceURL, "https://archive.org/details/") {
		t.Fatalf("unexpected audio source URL: %q", pair.AudioSourceURL)
	}
	if strings.Contains(pair.AudioSourceURL, "compress") || strings.Contains(pair.AudioSourceURL, "zip") {
		t.Fatalf("private download URL leaked in search response: %q", pair.AudioSourceURL)
	}
}

func TestArchiveSearchSupportsShortTitlesWithOneDistinctiveWord(t *testing.T) {
	pgCatalog := gzipPGCatalog(t, `14082,Text,2006-06-01,The Raven,en,"Poe, Edgar Allan, 1809-1849",Poetry,PS,Classics`+"\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/advancedsearch.php":
			if r.URL.Query().Get("q") != "collection:librivoxaudio AND mediatype:audio AND title:(raven)" {
				t.Errorf("unexpected query: %q", r.URL.Query().Get("q"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"docs": []any{map[string]any{
				"identifier": "raven", "title": "The Raven", "creator": "Edgar Allan Poe",
				"language": "eng", "source": "Librivox recording of Gutenberg e-text 14082",
			}}}})
		case "/pg_catalog.csv.gz":
			_, _ = w.Write(pgCatalog)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.ArchiveEnabled = true
	client.ArchiveBaseURL = server.URL
	client.GutenbergCatalogURL = server.URL + "/pg_catalog.csv.gz"
	client.HTTP = server.Client()
	pairs, err := client.Search(context.Background(), "The Raven")
	if err != nil || len(pairs) != 1 || pairs[0].GutenbergID != "14082" {
		t.Fatalf("short distinctive title search = %#v, err=%v", pairs, err)
	}
}

func TestLibriVoxAPIIsUsedWhenArchiveSearchIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/advancedsearch.php":
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		case "/api/feed/audiobooks/":
			_ = json.NewEncoder(w).Encode(Response{Books: []Record{{
				ID: "391", Title: "The Gift of the Magi", Language: "English",
				URLTextSource: "https://www.gutenberg.org/ebooks/7256",
				URLZipFile:    "https://archive.org/compress/giftofmagi/formats=64KBPS%20MP3",
				URLLibriVox:   "https://librivox.org/the-gift-of-the-magi/",
				Authors:       []Author{{FirstName: "O.", LastName: "Henry"}},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClient()
	client.ArchiveEnabled = true
	client.ArchiveBaseURL = server.URL
	client.ArchiveRequestGap = 0
	client.BaseURL = server.URL + "/api/feed/audiobooks/"
	client.minRequestGap = 0
	client.HTTP = server.Client()
	pairs, err := client.Search(context.Background(), "Gift of Magi")
	if err != nil || len(pairs) != 1 || pairs[0].Provider != "librivox" || pairs[0].GutenbergID != "7256" {
		t.Fatalf("LibriVox fallback = %#v, err=%v", pairs, err)
	}
}

func TestArchiveTitleAuthorMatchReturnsEveryAmbiguousText(t *testing.T) {
	pgCatalog := gzipPGCatalog(t,
		`45,Text,2008-06-27,Anne of Green Gables,en,"Montgomery, L. M. (Lucy Maud), 1874-1942",Fiction,PZ,Classics`+"\n"+
			`19576,Text,2006-10-20,Anne of Green Gables,en,"Montgomery, L. M. (Lucy Maud), 1874-1942",Fiction,PZ,Classics`+"\n"+
			`19577,Sound,2006-10-20,Anne of Green Gables,en,"Montgomery, L. M. (Lucy Maud), 1874-1942",Fiction,PZ,Classics`+"\n",
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/advancedsearch.php":
			_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"docs": []any{map[string]any{
				"identifier": "anne_of_green_gables_librivox", "title": "Anne of Green Gables",
				"creator": "Lucy Maud Montgomery", "language": "eng", "runtime": "10:30.11",
				"source": "Librivox recording of a public-domain text",
			}}}})
		case "/pg_catalog.csv.gz":
			_, _ = w.Write(pgCatalog)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.ArchiveEnabled = true
	client.ArchiveBaseURL = server.URL
	client.GutenbergCatalogURL = server.URL + "/pg_catalog.csv.gz"
	client.HTTP = server.Client()
	pairs, err := client.Search(context.Background(), "Anne Green Gables")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 {
		t.Fatalf("got %d candidate pairs: %#v", len(pairs), pairs)
	}
	pair := pairs[0]
	if pair.MatchKind != "title_author" || pair.GutenbergID != "" || len(pair.TextCandidates) != 2 {
		t.Fatalf("ambiguous matching was silently selected: %#v", pair)
	}
	chosen, ok := SelectTextCandidate(pair, "45")
	if !ok || chosen.GutenbergID != "45" || chosen.MatchKind != "title_author" {
		t.Fatalf("valid text choice rejected: %#v %v", chosen, ok)
	}
	if _, ok := SelectTextCandidate(pair, "22440"); ok {
		t.Fatal("an unlisted Gutenberg ID was accepted")
	}
	if got := pair.DurationMS; got != (10*3600+30*60+11)*1000 {
		t.Fatalf("duration = %dms, want %dms", got, (10*3600+30*60+11)*1000)
	}
}

func TestArchiveDescriptionCrosswalkIgnoresUnrelatedTextLinks(t *testing.T) {
	index := &gutenbergIndex{byID: map[string]gutenbergEntry{
		"759": {ID: "759", Title: "James Pethel", Authors: "Beerbohm, Max, Sir, 1872-1956", Language: "en"},
		"761": {ID: "761", Title: "A. V. Laider", Authors: "Beerbohm, Max, Sir, 1872-1956", Language: "en"},
	}}
	record := Record{
		Provider: "internet_archive", Title: "Seven Men", Language: "en",
		Authors:     []Author{{FirstName: "Max Beerbohm"}},
		ArchiveDesc: `Note that the Gutenberg edition is incomplete: <a href="https://www.gutenberg.org/ebooks/759">James Pethel</a> and <a href="https://www.gutenberg.org/ebooks/761">E.V. Laider</a>.`,
	}
	if candidates := textCandidatesForArchiveRecord(record, index); len(candidates) != 0 {
		t.Fatalf("unrelated anthology texts were accepted as the full book: %#v", candidates)
	}
	if got := linkedGutenbergIDs("Librivox recording of Gutenberg e-text#6753", "https://www.gutenberg.org/etext/45"); len(got) != 2 ||
		got[0] != "45" || got[1] != "6753" {
		t.Fatalf("unexpected extracted Gutenberg IDs: %#v", got)
	}
}

func TestArchiveDescriptionLinksToMatchingGutenbergText(t *testing.T) {
	index := &gutenbergIndex{byID: map[string]gutenbergEntry{
		"3533": {ID: "3533", Title: "Sunshine Sketches of a Little Town", Authors: "Leacock, Stephen, 1869-1944", Language: "en"},
	}}
	record := Record{
		Provider: "internet_archive", Title: "Sunshine Sketches of a Little Town", Language: "en",
		Authors:     []Author{{FirstName: "Stephen Leacock"}},
		ArchiveDesc: `From the Gutenberg e-text: <a href="https://www.gutenberg.org/ebooks/3533">Sunshine Sketches</a>.`,
	}
	candidates := textCandidatesForArchiveRecord(record, index)
	if len(candidates) != 1 || candidates[0].GutenbergID != "3533" || candidates[0].MatchBasis != "source_linked" {
		t.Fatalf("matching IA description link was not preserved: %#v", candidates)
	}
}

func TestArchiveByIDRevalidatesLibriVoxCollectionAndMP3(t *testing.T) {
	pgCatalog := gzipPGCatalog(t, `7256,Text,2005-01-01,The Gift of the Magi,en,"Henry, O., 1862-1910",Stories,PS,Classics`+"\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metadata/giftofmagi":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{
					"identifier": "giftofmagi", "title": "The Gift of the Magi", "creator": "O. Henry",
					"language": "eng", "mediatype": "audio", "collection": []string{"librivoxaudio"},
					"source": "Librivox recording of Gutenberg e-text #7256",
				},
				"files": []any{map[string]any{"name": "gift_64kb.mp3", "private": false}},
			})
		case "/pg_catalog.csv.gz":
			_, _ = w.Write(pgCatalog)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.ArchiveBaseURL = server.URL
	client.GutenbergCatalogURL = server.URL + "/pg_catalog.csv.gz"
	client.HTTP = server.Client()
	record, err := client.ByID(context.Background(), "ia-giftofmagi")
	if err != nil {
		t.Fatal(err)
	}
	pair, ok := ToPair(record)
	if !ok || pair.Provider != "internet_archive" || pair.GutenbergID != "7256" {
		t.Fatalf("item did not revalidate as a paired source: %#v %v", pair, ok)
	}
	if _, err := client.ByID(context.Background(), "ia-../private"); err == nil {
		t.Fatal("unsafe item identifier was accepted")
	}
}

func TestArchiveByIDRejectsOtherCollectionsAndPrivateAudio(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identifier := strings.TrimPrefix(r.URL.Path, "/metadata/")
		collection := []string{"librivoxaudio"}
		private := false
		if identifier == "otheraudio" {
			collection = []string{"audio_bookspoetry"}
		}
		if identifier == "privateaudio" {
			private = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{
				"identifier": identifier, "title": "A public title", "creator": "A. Author",
				"mediatype": "audio", "collection": collection,
			},
			"files": []any{map[string]any{"name": "chapter.mp3", "private": private}},
		})
	}))
	defer server.Close()
	client := NewClient()
	client.ArchiveBaseURL = server.URL
	client.ArchiveRequestGap = 0
	client.HTTP = server.Client()
	for _, id := range []string{"ia-otheraudio", "ia-privateaudio"} {
		if _, err := client.ByID(context.Background(), id); err == nil {
			t.Errorf("accepted ineligible IA item %q", id)
		}
	}
}

func TestParseArchiveDuration(t *testing.T) {
	for input, want := range map[string]int64{
		"8:37:32":  31_052,
		"10:30.11": 37_811,
		"13:22":    802,
	} {
		if got := parseArchiveDuration(input); got != want {
			t.Errorf("parseArchiveDuration(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestGutenbergCatalogDiskCacheAvoidsRepeatDownloads(t *testing.T) {
	pgCatalog := gzipPGCatalog(t, `7256,Text,2005-01-01,The Gift of the Magi,en,"Henry, O., 1862-1910",Stories,PS,Classics`+"\n")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(pgCatalog)
	}))
	defer server.Close()
	cacheDir := filepath.Join(t.TempDir(), "catalog")

	first := NewClient()
	first.GutenbergCatalogURL = server.URL
	first.CatalogCacheDir = cacheDir
	first.HTTP = server.Client()
	if _, err := first.projectGutenbergIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := NewClient()
	second.GutenbergCatalogURL = "https://gutenberg.invalid/not-used"
	second.CatalogCacheDir = cacheDir
	if index, err := second.projectGutenbergIndex(context.Background()); err != nil || index.byID["7256"].Title != "The Gift of the Magi" {
		t.Fatalf("disk catalog cache failed: index=%#v err=%v", index, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("catalog fetched %d times, want one", requests.Load())
	}
}

func TestArchiveNarratorAndAudioVariantTitle(t *testing.T) {
	if got := archiveNarrator(`LibriVox recording. Read by <a href="https://example.org/reader">Karen Savage</a>.`); got != "Karen Savage" {
		t.Fatalf("narrator = %q", got)
	}
	if !equalTokens(cleanMatchTitle("Anne of Green Gables (Version 7) (dramatic reading)"), cleanMatchTitle("Anne of Green Gables")) {
		t.Fatal("audio variant annotations prevented matching the underlying title")
	}
	if got := archiveLibriVoxPageURL(`Catalog page: <a href="http://librivox.org/anne-of-green-gables-by-lucy-maud-montgomery/">LibriVox</a>`); got != "https://librivox.org/anne-of-green-gables-by-lucy-maud-montgomery" {
		t.Fatalf("LibriVox detail page URL = %q", got)
	}
	if got := archiveLibriVoxPageURL(`Links: https://librivox.org/rss/146 and https://librivox.org.evil.invalid/book`); got != "" {
		t.Fatalf("accepted non-catalog LibriVox link %q", got)
	}
}
