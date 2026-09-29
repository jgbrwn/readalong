package media

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DownloadURL fetches a direct media URL without using proxy environment
// variables and pins each connection to an address verified as public.
func DownloadURL(ctx context.Context, rawURL, dest string, maxBytes int64) error {
	u, err := parseHTTPURL(rawURL)
	if err != nil {
		return err
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
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if _, err := parseHTTPURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("invalid media URL")
	}
	req.Header.Set("User-Agent", "Readalong/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not fetch direct media URL")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("media server returned HTTP %d", resp.StatusCode)
	}
	if maxBytes <= 0 {
		return fmt.Errorf("invalid download limit")
	}
	if resp.ContentLength > maxBytes {
		return fmt.Errorf("media download exceeds the configured size limit")
	}
	if !isSupportedMediaType(resp.Header.Get("Content-Type")) {
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
