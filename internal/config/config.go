package config

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Env                  string
	Addr                 string
	DataDir              string
	BaseURL              string
	RequireExe           bool
	DenyStatus           int
	DevUserID            string
	DevUserEmail         string
	AdminUserIDs         map[string]bool
	AdminBootstrapEmails map[string]bool
	GroqAPIKey           string
	GroqModel            string
	GroqLanguage         string
	GroqChunkSeconds     int
	GroqOverlapSeconds   int
	MaxUploadBytes       int64
	FFmpegBin            string
	FFprobeBin           string
	YTDLPBin             string
	DenoBin              string
}

func Load() Config {
	loadDotEnv(".env")
	cfg := Config{
		Env:                  env("APP_ENV", "production"),
		Addr:                 loopbackAddr(env("APP_PORT", "8000")),
		DataDir:              env("APP_DATA_DIR", "./data"),
		BaseURL:              strings.TrimRight(env("APP_BASE_URL", ""), "/"),
		RequireExe:           envBool("AUTH_REQUIRE_EXE", true),
		DenyStatus:           envInt("AUTH_DENY_STATUS", 404),
		DevUserID:            env("DEV_USER_ID", "dev-user"),
		DevUserEmail:         strings.ToLower(env("DEV_USER_EMAIL", "dev@example.com")),
		AdminUserIDs:         csvSet(os.Getenv("ADMIN_USER_IDS")),
		AdminBootstrapEmails: lowerSet(os.Getenv("ADMIN_BOOTSTRAP_EMAILS")),
		GroqAPIKey:           os.Getenv("GROQ_API_KEY"),
		GroqModel:            env("GROQ_MODEL", "whisper-large-v3-turbo"),
		GroqLanguage:         env("GROQ_LANGUAGE", "en"),
		GroqChunkSeconds:     envInt("GROQ_CHUNK_SECONDS", 480),
		GroqOverlapSeconds:   envInt("GROQ_CHUNK_OVERLAP_SECONDS", 2),
		MaxUploadBytes:       envInt64("MAX_UPLOAD_BYTES", 2<<30),
		FFmpegBin:            env("FFMPEG_BIN", "ffmpeg"),
		FFprobeBin:           env("FFPROBE_BIN", "ffprobe"),
		YTDLPBin:             env("YTDLP_BIN", "yt-dlp"),
		DenoBin:              env("DENO_BIN", ""),
	}
	stripDeploymentSecrets()
	return cfg.Normalize()
}

// Normalize resolves the persistent data root once so paths remain stable
// when subprocesses such as FFmpeg run with a different working directory.
func (c Config) Normalize() Config {
	if strings.TrimSpace(c.DataDir) == "" {
		return c
	}
	if absolute, err := filepath.Abs(c.DataDir); err == nil {
		c.DataDir = absolute
	} else {
		c.DataDir = filepath.Clean(c.DataDir)
	}
	return c
}

// API credentials are copied into the narrow consumers that need them, not
// inherited by ffmpeg, ffprobe, yt-dlp, or other child processes.
func stripDeploymentSecrets() {
	for _, key := range []string{
		"GROQ_API_KEY", "CLOUDFLARE_API_TOKEN", "R2_ACCOUNT_ID", "R2_ACCESS_KEY_ID",
		"R2_SECRET_ACCESS_KEY", "R2_ENDPOINT",
	} {
		_ = os.Unsetenv(key)
	}
}

func loopbackAddr(port string) string {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		port = "8000"
	}
	return net.JoinHostPort("127.0.0.1", port)
}

func env(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}
func envBool(k string, d bool) bool {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return d
	}
	b, e := strconv.ParseBool(v)
	if e != nil {
		return d
	}
	return b
}
func envInt(k string, d int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return d
	}
	n, e := strconv.Atoi(v)
	if e != nil {
		return d
	}
	return n
}
func envInt64(k string, d int64) int64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return d
	}
	n, e := strconv.ParseInt(v, 10, 64)
	if e != nil {
		return d
	}
	return n
}
func csvSet(v string) map[string]bool {
	out := map[string]bool{}
	for _, x := range strings.Split(v, ",") {
		x = strings.TrimSpace(x)
		if x != "" {
			out[x] = true
		}
	}
	return out
}

func lowerSet(v string) map[string]bool {
	out := map[string]bool{}
	for x := range csvSet(v) {
		out[strings.ToLower(x)] = true
	}
	return out
}

// loadDotEnv reads simple KEY=value entries without overriding the process
// environment. Systemd EnvironmentFile and explicitly exported variables
// therefore remain authoritative.
func loadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			continue
		}
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}
