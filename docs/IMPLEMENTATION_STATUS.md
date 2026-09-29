# Implementation status

## Audio-only phase

Implemented:

- exe.dev-header identity, first-visit account creation, stable-ID ownership,
  and runtime-configured admin access;
- secure localhost-only server configuration and systemd installation;
- audio upload, YouTube acquisition, and SSRF-protected direct media download;
- ffprobe metadata, MP3 playback normalization, chunking, and persistent jobs;
- Groq word timestamps, overlap merging, atomic transcript files, rate-limit
  queueing, and restart recovery;
- bookshelf, Range-capable audio, SSE processing events, progress persistence,
  and the mobile-first PWA reader.

Run `go test ./...`, `go vet ./...`, and `make build` before deployment. Live
Groq and YouTube smoke tests are deliberately not part of automated tests.

## Not implemented yet

- EPUB extraction and quality-gated audio/text alignment;
- generic yt-dlp webpage extraction beyond YouTube and direct public media;
- R2 asset mirroring, Litestream credentials, and a verified restore drill;
- cover extraction, search, bookmarks, and offline audio downloads.

Each installation should verify its own private exe.dev share and configure
secrets locally. Public source availability does not imply a public running
service.
