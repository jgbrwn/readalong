package media

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Tools struct {
	YTDLP, FFMPEG, FFPROBE, Deno string
	MaxBytes                     int64
}

type SourceMetadata struct {
	Title    string  `json:"title"`
	Uploader string  `json:"uploader"`
	Duration float64 `json:"duration"`
	Chapters []struct {
		Title     string  `json:"title"`
		StartTime float64 `json:"start_time"`
		EndTime   float64 `json:"end_time"`
	} `json:"chapters"`
}

// AcquireWithYTDLP follows the cookie-free YouTube strategy used by
// jgbrwn/mst3k-anything. Other URLs use DownloadURL, which enforces SSRF checks.
func (t Tools) AcquireWithYTDLP(ctx context.Context, rawURL, dir string) error {
	if !isYouTubeURL(rawURL) {
		return fmt.Errorf("only YouTube URLs are supported by yt-dlp")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("could not create source directory")
	}
	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 2 << 30
	}
	out := filepath.Join(dir, "source.%(ext)s")
	args := []string{"--ignore-config", "--no-playlist", "--no-warnings", "--max-filesize",
		fmt.Sprintf("%d", maxBytes), "-f", "bestaudio[ext=m4a]/bestaudio/best",
		"--write-info-json", "--remote-components", "ejs:github"}
	if t.Deno != "" {
		args = append(args, "--js-runtimes", "deno:"+t.Deno)
	}
	args = append(args, "-o", out, rawURL)
	cmd := exec.CommandContext(ctx, t.YTDLP, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = output // extractor output may contain signed URLs; do not expose it.
		retry := append([]string{"--extractor-args", "youtube:player_client=android"}, args...)
		if _, retryErr := exec.CommandContext(ctx, t.YTDLP, retry...).CombinedOutput(); retryErr != nil {
			return fmt.Errorf("could not acquire audio from YouTube")
		}
	}
	path, err := FindYTDLPSource(dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("downloaded source is unavailable")
	}
	if info.Size() > maxBytes {
		_ = os.Remove(path)
		_ = os.Remove(path + ".info.json")
		return fmt.Errorf("download exceeds the configured size limit")
	}
	return nil
}

func FindYTDLPSource(dir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "source.*"))
	if err != nil {
		return "", err
	}
	for _, path := range matches {
		if strings.HasSuffix(path, ".info.json") || strings.HasSuffix(path, ".part") {
			continue
		}
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
			_ = os.Chmod(path, 0600)
			return path, nil
		}
	}
	return "", fmt.Errorf("YouTube did not produce an audio file")
}

func ReadYTDLPMetadata(dir string) (SourceMetadata, error) {
	var m SourceMetadata
	matches, _ := filepath.Glob(filepath.Join(dir, "source.*.info.json"))
	if len(matches) == 0 {
		matches, _ = filepath.Glob(filepath.Join(dir, "source.info.json"))
	}
	if len(matches) == 0 {
		return m, nil
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return m, err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return m, err
	}
	return m, nil
}

func isYouTubeURL(raw string) bool {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	switch host {
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com", "youtu.be":
		return true
	default:
		return false
	}
}

func ClassifyURL(raw string) (kind, normalized string, err error) {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return "", "", err
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		host == "localhost" || (net.ParseIP(host) != nil && !isPublicIP(net.ParseIP(host))) {
		return "", "", fmt.Errorf("URL host must be public")
	}
	u.Fragment = ""
	if isYouTubeURL(u.String()) {
		return "youtube", u.String(), nil
	}
	return "url", u.String(), nil
}

func (t Tools) Normalize(ctx context.Context, input, output string) error {
	cmd := exec.CommandContext(ctx, t.FFMPEG, "-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "file,crypto,data",
		"-y", "-i", input, "-vn", "-map_metadata", "0",
		"-c:a", "libmp3lame", "-b:a", "96k", "-ac", "1", "-ar", "44100", output)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = out
		return fmt.Errorf("could not normalize audio")
	}
	return nil
}

func (t Tools) CreateChunk(ctx context.Context, input, output string, startMS, endMS int64) error {
	duration := float64(endMS-startMS) / 1000
	start := float64(startMS) / 1000
	cmd := exec.CommandContext(ctx, t.FFMPEG, "-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "file,crypto,data", "-y", "-ss", fmt.Sprintf("%.3f", start),
		"-i", input, "-t", fmt.Sprintf("%.3f", duration),
		"-vn", "-ac", "1", "-ar", "16000", "-c:a", "flac", "-compression_level", "5", output)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = out
		return fmt.Errorf("could not prepare transcription chunk")
	}
	return nil
}

type ProbeResult struct {
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Chapters []struct {
		StartTime string            `json:"start_time"`
		EndTime   string            `json:"end_time"`
		Tags      map[string]string `json:"tags"`
	} `json:"chapters"`
}

func (t Tools) Probe(ctx context.Context, path string) (ProbeResult, error) {
	var result ProbeResult
	cmd := exec.CommandContext(ctx, t.FFPROBE, "-protocol_whitelist", "file,crypto,data", "-v", "error", "-show_entries",
		"format=duration:format_tags=title,artist,album:chapter=start_time,end_time:chapter_tags=title",
		"-of", "json", path)
	out, err := cmd.Output()
	if err != nil {
		return result, fmt.Errorf("could not inspect audio metadata")
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return result, fmt.Errorf("could not parse audio metadata")
	}
	return result, nil
}

func ValidateAudioFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("media source is missing")
	}
	defer f.Close()
	var header [16]byte
	n, err := io.ReadFull(f, header[:])
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return fmt.Errorf("could not inspect media source")
	}
	h := header[:n]
	valid := bytesPrefix(h, "ID3") ||
		len(h) >= 2 && h[0] == 0xff && h[1]&0xe0 == 0xe0 ||
		bytesPrefix(h, "fLaC") || bytesPrefix(h, "OggS") ||
		len(h) >= 12 && string(h[:4]) == "RIFF" && string(h[8:12]) == "WAVE" ||
		len(h) >= 8 && string(h[4:8]) == "ftyp" ||
		len(h) >= 4 && string(h[:4]) == "\x1a\x45\xdf\xa3"
	if !valid {
		return fmt.Errorf("media source is not a supported audio file")
	}
	return nil
}

func bytesPrefix(value []byte, prefix string) bool {
	return len(value) >= len(prefix) && string(value[:len(prefix)]) == prefix
}
