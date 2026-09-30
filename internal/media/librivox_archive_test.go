package media

import (
	"archive/zip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestJoinLibriVoxArchive(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	root := t.TempDir()
	var tracks = []struct {
		name, frequency string
	}{
		{"Chapter 10.mp3", "900"},
		{"Chapter 2.mp3", "300"},
	}
	zipPath := filepath.Join(root, "book.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	for i, track := range tracks {
		mp3Path := filepath.Join(root, "input-"+string(rune('0'+i))+".mp3")
		cmd := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "sine=frequency="+track.frequency+":duration=0.3",
			"-c:a", "libmp3lame", "-b:a", "64k", mp3Path)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("make fixture MP3: %v: %s", err, output)
		}
		content, err := os.ReadFile(mp3Path)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := zw.Create(track.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(root, "joined.mp3")
	if err := JoinLibriVoxArchive(context.Background(), zipPath, outputPath,
		filepath.Join(root, "work"), ffmpeg, 2<<20); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAudioFile(outputPath); err != nil {
		t.Fatalf("joined output is not valid MP3: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "work")); !os.IsNotExist(err) {
		t.Fatalf("chapter work data was not cleaned: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeRoot, err := filepath.Rel(cwd, root)
	if err != nil {
		t.Fatal(err)
	}
	relativeOutput := filepath.Join(relativeRoot, "joined-relative.mp3")
	relativeWork := filepath.Join(relativeRoot, "work-relative")
	if err := JoinLibriVoxArchive(context.Background(), filepath.Join(relativeRoot, "book.zip"),
		relativeOutput, relativeWork, ffmpeg, 2<<20); err != nil {
		t.Fatalf("join with relative data paths: %v", err)
	}
	if err := ValidateAudioFile(relativeOutput); err != nil {
		t.Fatalf("relative-path joined output is not valid MP3: %v", err)
	}
	if _, err := os.Stat(relativeWork); !os.IsNotExist(err) {
		t.Fatalf("relative chapter work data was not cleaned: %v", err)
	}
}

func TestJoinLibriVoxArchiveRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "bad.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	entry, err := zw.Create("../outside.mp3")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("not really an mp3"))
	_ = zw.Close()
	_ = file.Close()
	if err := JoinLibriVoxArchive(context.Background(), archivePath, filepath.Join(root, "joined.mp3"),
		filepath.Join(root, "work"), "ffmpeg", 1<<20); err == nil {
		t.Fatal("unsafe path accepted")
	}
}
