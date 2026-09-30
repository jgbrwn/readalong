package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxDownloadAttempts = 2
	downloadRetryDelay  = 500 * time.Millisecond
	maxRetryAfterWait   = 15 * time.Second
)

// DownloadURL fetches a direct media URL without using proxy environment
// variables and pins each connection to an address verified as public.
func DownloadURL(ctx context.Context, rawURL, dest string, maxBytes int64) error {
	return downloadPublic(ctx, rawURL, dest, maxBytes, parseHTTPURL, isSupportedMediaType)
}

func DownloadLibriVoxArchive(ctx context.Context, rawURL, dest string, maxBytes int64) error {
	return downloadPublic(ctx, rawURL, dest, maxBytes, validateLibriVoxArchiveURL, isZipContentType)
}

func DownloadGutenbergEPUB(ctx context.Context, id, dest string, maxBytes int64) error {
	target, expectedPath, err := GutenbergEPUBURL(id)
	if err != nil {
		return err
	}
	validate := func(raw string) (*url.URL, error) {
		u, err := parseHTTPURL(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() != "gutenberg.pglaf.org" ||
			u.Path != expectedPath || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("unsupported Project Gutenberg EPUB URL")
		}
		return u, nil
	}
	return downloadPublic(ctx, target, dest, maxBytes, validate, isEPUBContentType)
}

func GutenbergEPUBURL(id string) (string, string, error) {
	if !digitsOnly(id) || len(id) > 12 {
		return "", "", fmt.Errorf("invalid Project Gutenberg ID")
	}
	expectedPath := "/cache/epub/" + id + "/pg" + id + ".epub"
	return "https://gutenberg.pglaf.org" + expectedPath, expectedPath, nil
}

func validateLibriVoxArchiveURL(raw string) (*url.URL, error) {
	u, err := parseHTTPURL(raw)
	if err != nil || u.Scheme != "https" || !isArchiveHost(u.Hostname()) {
		return nil, fmt.Errorf("unsupported LibriVox archive URL")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if strings.HasPrefix(u.Path, "/compress/") &&
		(host == "archive.org" || host == "www.archive.org") && u.RawQuery == "" {
		return u, nil
	}
	if strings.HasPrefix(u.Path, "/download/") && u.RawQuery == "" {
		return u, nil
	}
	if u.Path == "/zip_dir.php" && strings.HasSuffix(host, ".archive.org") {
		query := u.Query()
		paths, formats := query["path"], query["formats"]
		if len(query) == 2 && len(paths) == 1 && len(formats) == 1 &&
			validArchiveShardZipPath(paths[0]) && strings.Contains(strings.ToUpper(formats[0]), "MP3") {
			return u, nil
		}
	}
	return nil, fmt.Errorf("unsupported LibriVox archive URL")
}

// Archive.org storage redirects may use numeric shard directories such as
// /0/items/ or /14/items/. Accept only one safe item ZIP path, not arbitrary
// paths or nested directories.
func validArchiveShardZipPath(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 4 || parts[0] != "" || parts[2] != "items" ||
		len(parts[1]) > 8 || !digitsOnly(parts[1]) {
		return false
	}
	filename := parts[3]
	if !strings.HasSuffix(strings.ToLower(filename), ".zip") {
		return false
	}
	identifier := filename[:len(filename)-len(".zip")]
	if identifier == "" || len(identifier) > 100 || identifier[0] == '.' || identifier[0] == '-' {
		return false
	}
	for _, r := range identifier {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

type urlValidator func(string) (*url.URL, error)
type contentTypeValidator func(string) bool

func downloadPublic(ctx context.Context, rawURL, dest string, maxBytes int64, validate urlValidator, accepts contentTypeValidator) error {
	u, err := validate(rawURL)
	if err != nil {
		return err
	}
	if maxBytes <= 0 {
		return fmt.Errorf("invalid download limit")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialPublic(ctx, network, address)
		},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Minute,
	}
	return downloadPublicWithClient(ctx, u, dest, maxBytes, validate, accepts, client)
}

// downloadPublicWithClient gives source downloads one bounded retry for
// transient transport/server errors. Production supplies a transport that
// pins every resolved address to a public IP.
func downloadPublicWithClient(ctx context.Context, u *url.URL, dest string, maxBytes int64, validate urlValidator, accepts contentTypeValidator, client *http.Client) error {
	if maxBytes <= 0 {
		return fmt.Errorf("invalid download limit")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	safeClient := *client
	safeClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if _, err := validate(req.URL.String()); err != nil {
			return err
		}
		if req.URL.Path == "/zip_dir.php" && isArchiveHost(req.URL.Hostname()) {
			req.URL.RawQuery = strings.ReplaceAll(req.URL.RawQuery, " ", "+")
		}
		return nil
	}
	for attempt := 0; attempt < maxDownloadAttempts; attempt++ {
		err := downloadPublicAttempt(ctx, &safeClient, u, dest, maxBytes, accepts)
		if err == nil {
			return nil
		}
		var retryErr *retryableDownloadError
		if attempt+1 == maxDownloadAttempts || !errors.As(err, &retryErr) {
			return err
		}
		delay := retryErr.delay
		if delay < downloadRetryDelay {
			delay = downloadRetryDelay
		}
		if delay > maxRetryAfterWait {
			return retryErr.cause
		}
		if err := waitForDownloadRetry(ctx, delay); err != nil {
			return fmt.Errorf("media download was canceled")
		}
	}
	return fmt.Errorf("could not fetch direct media URL")
}

type retryableDownloadError struct {
	cause error
	delay time.Duration
}

func (e *retryableDownloadError) Error() string { return e.cause.Error() }
func (e *retryableDownloadError) Unwrap() error { return e.cause }

func downloadPublicAttempt(ctx context.Context, client *http.Client, u *url.URL, dest string, maxBytes int64, accepts contentTypeValidator) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("invalid media URL")
	}
	req.Header.Set("User-Agent", "Readalong/1.0")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil && isTransientTransferError(err) {
			return &retryableDownloadError{cause: fmt.Errorf("could not fetch direct media URL"), delay: downloadRetryDelay}
		}
		return fmt.Errorf("could not fetch direct media URL")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := fmt.Errorf("media server returned HTTP %d", resp.StatusCode)
		if retry, delay := retryableDownloadStatus(resp.StatusCode, resp.Header.Get("Retry-After")); retry {
			return &retryableDownloadError{cause: failure, delay: delay}
		}
		return failure
	}
	if resp.ContentLength > maxBytes {
		return fmt.Errorf("media download exceeds the configured size limit")
	}
	if !accepts(resp.Header.Get("Content-Type")) {
		return fmt.Errorf("URL did not return an audio or supported media file")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return fmt.Errorf("could not create media directory")
	}
	tmp := dest + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("could not create media file")
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if copyErr != nil && syncErr == nil && closeErr == nil && ctx.Err() == nil &&
			isTransientTransferError(copyErr) {
			return &retryableDownloadError{cause: fmt.Errorf("media transfer was interrupted"), delay: downloadRetryDelay}
		}
		return fmt.Errorf("could not save media file")
	}
	if n > maxBytes {
		_ = os.Remove(tmp)
		return fmt.Errorf("media download exceeds the configured size limit")
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("could not finalize media file")
	}
	return nil
}

func retryableDownloadStatus(status int, retryAfter string) (bool, time.Duration) {
	switch {
	case status == http.StatusTooManyRequests:
		delay, ok := parseDownloadRetryAfter(retryAfter)
		return ok && delay <= maxRetryAfterWait, delay
	case status == http.StatusRequestTimeout:
		return retryDelayWithinLimit(retryAfter)
	case status >= 500 && status <= 599 &&
		status != http.StatusNotImplemented && status != http.StatusHTTPVersionNotSupported:
		return retryDelayWithinLimit(retryAfter)
	default:
		return false, 0
	}
}

func retryDelayWithinLimit(retryAfter string) (bool, time.Duration) {
	delay, ok := parseDownloadRetryAfter(retryAfter)
	if ok {
		return delay <= maxRetryAfterWait, delay
	}
	return true, downloadRetryDelay
}

func parseDownloadRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		if seconds > int64(maxRetryAfterWait/time.Second) {
			return maxRetryAfterWait + time.Second, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := time.Until(when)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func isTransientTransferError(err error) bool {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary())
}

func waitForDownloadRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isArchiveHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "archive.org" || host == "www.archive.org" || strings.HasSuffix(host, ".archive.org")
}

func isZipContentType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	return value == "application/zip" || value == "application/x-zip-compressed" ||
		value == "application/octet-stream"
}

func isEPUBContentType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	return value == "application/epub+zip" || value == "application/zip" ||
		value == "application/octet-stream"
}

func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" {
		return nil, fmt.Errorf("only public HTTP(S) media URLs are supported")
	}
	return u, nil
}

func isSupportedMediaType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	return strings.HasPrefix(value, "audio/") ||
		value == "application/octet-stream" ||
		value == "video/mp4" || value == "video/webm"
}

func dialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid destination")
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(resolved) == 0 {
			return nil, fmt.Errorf("could not resolve media host")
		}
		for _, addr := range resolved {
			ips = append(ips, addr.IP)
		}
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("media host resolves to a non-public address")
		}
	}
	dialer := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func isPublicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() ||
		addr.IsUnspecified() {
		return false
	}
	// Shared address space and benchmarking networks are not public egress.
	for _, cidr := range []string{"100.64.0.0/10", "198.18.0.0/15"} {
		prefix, _ := netip.ParsePrefix(cidr)
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}
