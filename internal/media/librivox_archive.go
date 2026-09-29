package media

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const maxArchiveEntries = 4000

func JoinLibriVoxArchive(ctx context.Context, archivePath, outputPath, workDir, ffmpeg string, maxBytes int64) error {
	if maxBytes <= 0 {
		return fmt.Errorf("invalid archive limit")
	}
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("LibriVox archive is invalid")
	}
	defer zr.Close()
	if len(zr.File) == 0 || len(zr.File) > maxArchiveEntries {
		return fmt.Errorf("LibriVox archive contains too many files")
	}
	var tracks []*zip.File
	var total uint64
	for _, f := range zr.File {
		if !safeArchiveEntry(f.Name) || f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("LibriVox archive contains an unsafe path")
		}
		if strings.EqualFold(filepath.Ext(f.Name), ".mp3") && !f.FileInfo().IsDir() {
			if f.UncompressedSize64 == 0 || f.UncompressedSize64 > uint64(maxBytes) ||
				total > uint64(maxBytes)-f.UncompressedSize64 {
				return fmt.Errorf("LibriVox archive expands beyond the configured size limit")
			}
			total += f.UncompressedSize64
			tracks = append(tracks, f)
		}
	}
	if len(tracks) == 0 {
		return fmt.Errorf("LibriVox archive has no MP3 chapters")
	}
	sort.SliceStable(tracks, func(i, j int) bool {
		return naturalLess(tracks[i].Name, tracks[j].Name)
	})
	if err := os.MkdirAll(workDir, 0700); err != nil {
		return fmt.Errorf("could not prepare chapter work directory")
	}
	defer os.RemoveAll(workDir)
	listPath := filepath.Join(workDir, "tracks.txt")
	list, err := os.OpenFile(listPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("could not prepare chapter list")
	}
	for i, entry := range tracks {
		name := fmt.Sprintf("track-%05d.mp3", i)
		trackPath := filepath.Join(workDir, name)
		if err := extractTrack(entry, trackPath, maxBytes); err != nil {
			_ = list.Close()
			return err
		}
		if _, err := fmt.Fprintf(list, "file '%s'\n", name); err != nil {
			_ = list.Close()
			return fmt.Errorf("could not prepare chapter list")
		}
	}
	if err := list.Sync(); err != nil {
		_ = list.Close()
		return fmt.Errorf("could not prepare chapter list")
	}
	if err := list.Close(); err != nil {
		return fmt.Errorf("could not prepare chapter list")
	}
	tmpPath := outputPath + ".partial.mp3"
	_ = os.Remove(tmpPath)
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "file,crypto,data", "-f", "concat", "-safe", "1", "-i", listPath,
		"-vn", "-c:a", "libmp3lame", "-b:a", "96k", "-ac", "1", "-ar", "44100", "-y", tmpPath)
	cmd.Dir = workDir
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = output
		_ = os.Remove(tmpPath)
		return fmt.Errorf("could not join LibriVox chapters")
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("could not finalize LibriVox audio")
	}
	if err := os.Rename(tmpPath, outputPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("could not finalize LibriVox audio")
	}
	return nil
}

func extractTrack(entry *zip.File, dest string, maxBytes int64) error {
	if entry.UncompressedSize64 > uint64(maxBytes) {
		return fmt.Errorf("LibriVox chapter exceeds the configured size limit")
	}
	rc, err := entry.Open()
	if err != nil {
		return fmt.Errorf("could not read LibriVox chapter")
	}
	defer rc.Close()
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("could not prepare LibriVox chapter")
	}
	n, copyErr := io.Copy(f, io.LimitReader(rc, maxBytes+1))
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || uint64(n) != entry.UncompressedSize64 {
		_ = os.Remove(dest)
		return fmt.Errorf("LibriVox chapter is incomplete")
	}
	return nil
}

func safeArchiveEntry(name string) bool {
	if name == "" || strings.Contains(name, "\\") || strings.ContainsRune(name, '\x00') ||
		strings.HasPrefix(name, "/") {
		return false
	}
	for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if part == ".." || part == "." {
			return false
		}
	}
	clean := path.Clean(name)
	return clean != ".." && !strings.HasPrefix(clean, "../")
}

func naturalLess(a, b string) bool {
	ar, br := []rune(strings.ToLower(a)), []rune(strings.ToLower(b))
	for i, j := 0, 0; i < len(ar) && j < len(br); {
		if ar[i] >= '0' && ar[i] <= '9' && br[j] >= '0' && br[j] <= '9' {
			ii, jj := i, j
			for ii < len(ar) && ar[ii] >= '0' && ar[ii] <= '9' {
				ii++
			}
			for jj < len(br) && br[jj] >= '0' && br[jj] <= '9' {
				jj++
			}
			an, _ := strconv.ParseUint(string(ar[i:ii]), 10, 64)
			bn, _ := strconv.ParseUint(string(br[j:jj]), 10, 64)
			if an != bn {
				return an < bn
			}
			if ii-i != jj-j {
				return ii-i < jj-j
			}
			i, j = ii, jj
			continue
		}
		if ar[i] != br[j] {
			return ar[i] < br[j]
		}
		i++
		j++
	}
	return len(ar) < len(br)
}
