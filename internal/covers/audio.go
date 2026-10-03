package covers

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jgbrwn/readalong/internal/epub"
)

const maxEmbeddedArtworkBytes = 16 << 20

type probeArtwork struct {
	Streams []struct {
		Index       int `json:"index"`
		Disposition struct {
			AttachedPicture int `json:"attached_pic"`
		} `json:"disposition"`
	} `json:"streams"`
}

func extractEmbeddedArtwork(ctx context.Context, ffprobe, ffmpeg, filename string) ([]byte, bool) {
	if strings.TrimSpace(ffprobe) == "" || strings.TrimSpace(ffmpeg) == "" || filename == "" {
		return nil, false
	}
	probe := exec.CommandContext(ctx, ffprobe, "-protocol_whitelist", "file,crypto,data",
		"-v", "error", "-show_entries", "stream=index:stream_disposition=attached_pic", "-of", "json", filename)
	probe.Stderr = io.Discard
	probeStdout, err := probe.StdoutPipe()
	if err != nil {
		return nil, false
	}
	if err := probe.Start(); err != nil {
		return nil, false
	}
	probeBytes, readErr := io.ReadAll(io.LimitReader(probeStdout, 1<<20+1))
	if readErr != nil || len(probeBytes) > 1<<20 {
		_ = probe.Process.Kill()
		_ = probe.Wait()
		return nil, false
	}
	err = probe.Wait()
	if err != nil {
		return nil, false
	}
	var result probeArtwork
	if json.Unmarshal(probeBytes, &result) != nil {
		return nil, false
	}
	index := -1
	for _, stream := range result.Streams {
		if stream.Disposition.AttachedPicture == 1 && stream.Index >= 0 {
			index = stream.Index
			break
		}
	}
	if index < 0 {
		return nil, false
	}
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-protocol_whitelist", "file,crypto,data",
		"-v", "error", "-i", filename, "-map", "0:"+strconv.Itoa(index), "-frames:v", "1",
		"-an", "-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1")
	cmd.Stderr = io.Discard
	artworkStdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false
	}
	if err := cmd.Start(); err != nil {
		return nil, false
	}
	artworkBytes, readErr := io.ReadAll(io.LimitReader(artworkStdout, maxEmbeddedArtworkBytes+1))
	if readErr != nil || len(artworkBytes) == 0 || len(artworkBytes) > maxEmbeddedArtworkBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, false
	}
	if err := cmd.Wait(); err != nil {
		return nil, false
	}
	normalized, err := epub.NormalizeCoverImage(artworkBytes)
	if err != nil || len(normalized) == 0 {
		return nil, false
	}
	return normalized, true
}
