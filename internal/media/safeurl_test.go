package media

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRejectsNonPublicAddressRanges(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "198.18.0.1", "::1", "fd00::1", "fe80::1",
	} {
		if isPublicIP(net.ParseIP(raw)) {
			t.Errorf("%s unexpectedly considered public", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !isPublicIP(net.ParseIP(raw)) {
			t.Errorf("%s unexpectedly considered non-public", raw)
		}
	}
}

func TestDownloadURLCannotReachLoopback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("must not be reached"))
	}))
	defer server.Close()
	dest := filepath.Join(t.TempDir(), "audio.mp3")
	err := DownloadURL(context.Background(), server.URL, dest, 1024)
	if err == nil {
		t.Fatal("expected loopback URL to be rejected")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("destination unexpectedly exists: %v", err)
	}
}

func TestParseHTTPURL(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd", "http://user:pass@example.com/a.mp3",
	} {
		if _, err := parseHTTPURL(raw); err == nil {
			t.Errorf("parseHTTPURL(%q) unexpectedly succeeded", raw)
		}
	}
	for _, raw := range []string{"https://cdn.example.org/audio.mp3", "https://cdn.example.org:8443/audio.mp3"} {
		if _, err := parseHTTPURL(raw); err != nil {
			t.Fatalf("valid HTTPS URL rejected (%s): %v", raw, err)
		}
	}
}

func TestGutenbergEPUBURLUsesFixedMirrorPath(t *testing.T) {
	got, path, err := GutenbergEPUBURL("45")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://gutenberg.pglaf.org/cache/epub/45/pg45.epub" ||
		path != "/cache/epub/45/pg45.epub" {
		t.Fatalf("Gutenberg URL/path = %q %q", got, path)
	}
	for _, id := range []string{"", "../45", "45/../1", "9999999999999"} {
		if _, _, err := GutenbergEPUBURL(id); err == nil {
			t.Errorf("accepted Gutenberg ID %q", id)
		}
	}
}

func TestDownloadPublicRetriesTransientServerFailureOnce(t *testing.T) {
	var calls int
	payload := []byte("ID3 test audiobook")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL + "/audio.mp3")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "audio.mp3")
	err = downloadPublicWithClient(context.Background(), u, dest, 1024, parseHTTPURL, isSupportedMediaType, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("transient HTTP failure made %d requests, want 2", calls)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("downloaded bytes = %q, want %q", got, payload)
	}
	if _, err := os.Stat(dest + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("partial file remains after successful retry: %v", err)
	}
}

func TestDownloadPublicRetriesRateLimitOnlyWithShortRetryAfter(t *testing.T) {
	var calls int
	payload := []byte("ID3 rate-limited audiobook")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL + "/audio.mp3")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "audio.mp3")
	err = downloadPublicWithClient(context.Background(), u, dest, 1024, parseHTTPURL, isSupportedMediaType, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("rate-limited download made %d requests, want 2", calls)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("retried download did not produce media: %v", err)
	}
}

func TestDownloadPublicRetriesInterruptedBodyOnce(t *testing.T) {
	var calls int
	payload := []byte("complete audiobook")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "audio/mpeg")
		if calls == 1 {
			w.Header().Set("Content-Length", "64")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial"))
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL + "/audio.mp3")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "audio.mp3")
	err = downloadPublicWithClient(context.Background(), u, dest, 1024, parseHTTPURL, isSupportedMediaType, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("interrupted body made %d requests, want 2", calls)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("downloaded bytes = %q, want %q", got, payload)
	}
}

func TestDownloadPublicCapsTransientRetriesAtTwoAttempts(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "temporarily unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL + "/audio.mp3")
	if err != nil {
		t.Fatal(err)
	}
	err = downloadPublicWithClient(context.Background(), u, filepath.Join(t.TempDir(), "audio.mp3"),
		1024, parseHTTPURL, isSupportedMediaType, server.Client())
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("persistent transient response error = %v", err)
	}
	if calls != maxDownloadAttempts {
		t.Fatalf("persistent 502 made %d requests, want %d", calls, maxDownloadAttempts)
	}
}

func TestDownloadPublicDoesNotRetryPermanentHTTPFailure(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL + "/missing.epub")
	if err != nil {
		t.Fatal(err)
	}
	err = downloadPublicWithClient(context.Background(), u, filepath.Join(t.TempDir(), "book.epub"),
		1024, parseHTTPURL, isEPUBContentType, server.Client())
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("permanent response error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("404 response made %d requests, want 1", calls)
	}
}

func TestDownloadRetryAfterPolicyIsBounded(t *testing.T) {
	for _, test := range []struct {
		status    int
		retry     string
		wantRetry bool
		wantDelay time.Duration
	}{
		{status: http.StatusBadGateway, wantRetry: true, wantDelay: downloadRetryDelay},
		{status: http.StatusTooManyRequests, wantRetry: false},
		{status: http.StatusTooManyRequests, retry: "2", wantRetry: true, wantDelay: 2 * time.Second},
		{status: http.StatusServiceUnavailable, retry: "30", wantRetry: false},
		{status: http.StatusServiceUnavailable, retry: "5", wantRetry: true, wantDelay: 5 * time.Second},
		{status: http.StatusNotFound, retry: "1", wantRetry: false},
	} {
		gotRetry, gotDelay := retryableDownloadStatus(test.status, test.retry)
		if gotRetry != test.wantRetry || gotRetry && gotDelay != test.wantDelay {
			t.Errorf("status=%d Retry-After=%q: got (%v, %v), want (%v, %v)",
				test.status, test.retry, gotRetry, gotDelay, test.wantRetry, test.wantDelay)
		}
	}
}

func TestLibriVoxArchiveURLIsLimitedToArchiveOrg(t *testing.T) {
	for _, raw := range []string{
		"https://archive.org/compress/book/formats=64KBPS%20MP3",
		"https://ia801.us.archive.org/download/book.zip",
		"https://dn801702.us.archive.org/zip_dir.php?path=%2F0%2Fitems%2Fbook.zip&formats=64KBPS%20MP3",
		"https://ia801900.us.archive.org/zip_dir.php?path=%2F14%2Fitems%2Fanneofgreengablesversion9_2605_librivox.zip&formats=64KBPS%20MP3",
	} {
		if _, err := validateLibriVoxArchiveURL(raw); err != nil {
			t.Errorf("rejected valid archive URL %q: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"https://archive.org.attacker.invalid/compress/book.zip",
		"http://archive.org/compress/book.zip",
		"https://archive.org/metadata/book",
		"https://dn801702.us.archive.org/zip_dir.php?path=%2F0%2Fitems%2Fbook.txt&formats=64KBPS%20MP3",
		"https://dn801702.us.archive.org/zip_dir.php?path=%2F14%2Fitems%2F..%2Fbook.zip&formats=64KBPS%20MP3",
		"https://dn801702.us.archive.org/zip_dir.php?path=%2Fx%2Fitems%2Fbook.zip&formats=64KBPS%20MP3",
		"https://dn801702.us.archive.org/zip_dir.php?path=%2F14%2Fitems%2Fbook.zip&formats=64KBPS%20MP3&other=https%3A%2F%2Fexample.invalid",
		"https://example.com/download/book.zip",
	} {
		if _, err := validateLibriVoxArchiveURL(raw); err == nil {
			t.Errorf("accepted unsupported archive URL %q", raw)
		}
	}
}

func TestIsYouTubeURL(t *testing.T) {
	for _, raw := range []string{
		"https://youtube.com/watch?v=x", "https://www.youtube.com/watch?v=x",
		"https://youtu.be/x", "https://music.youtube.com/watch?v=x",
	} {
		if !isYouTubeURL(raw) {
			t.Errorf("expected YouTube URL: %s", raw)
		}
	}
	for _, raw := range []string{
		"https://youtube.com.evil.example/watch?v=x", "http://127.0.0.1/watch?v=x",
		"https://example.com/watch?v=x",
	} {
		if isYouTubeURL(raw) {
			t.Errorf("unexpected YouTube URL: %s", raw)
		}
	}
}

func TestClassifyURLRejectsPrivateLiteralHosts(t *testing.T) {
	for _, raw := range []string{
		"http://localhost/audio.mp3", "http://10.0.0.8/audio.mp3",
		"http://169.254.169.254/latest/meta-data", "http://[::1]/audio.mp3",
		"http://printer.local/audio.mp3",
	} {
		if _, _, err := ClassifyURL(raw); err == nil {
			t.Errorf("accepted private URL %s", raw)
		}
	}
}

func TestValidateAudioFileRejectsPlaylistAndHTMLPayloads(t *testing.T) {
	for _, content := range []string{"#EXTM3U\nhttp://127.0.0.1/audio\n", "<html>no audio</html>"} {
		path := filepath.Join(t.TempDir(), "source.download")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ValidateAudioFile(path); err == nil {
			t.Fatalf("accepted non-audio content %q", content)
		}
	}
}

func TestValidateAudioFileRecognizesCommonHeaders(t *testing.T) {
	for _, header := range [][]byte{
		[]byte("ID3\x04\x00\x00\x00\x00\x00\x00"),
		[]byte("fLaC"),
		[]byte("OggS"),
		[]byte("RIFF\x00\x00\x00\x00WAVE"),
		[]byte("\x00\x00\x00\x18ftypM4A "),
		[]byte("\x1a\x45\xdf\xa3\x00\x00\x00\x00"),
	} {
		path := filepath.Join(t.TempDir(), "audio")
		if err := os.WriteFile(path, header, 0600); err != nil {
			t.Fatal(err)
		}
		if err := ValidateAudioFile(path); err != nil {
			t.Errorf("rejected expected audio header %q: %v", header, err)
		}
	}
}
