# Research notes (snapshot: 2026-09-28)

Provider models, upload limits, pricing, and rate limits change. Treat this
document as historical context and verify the linked primary documentation
before depending on a numeric limit. The application does not hard-code Groq
quotas; it persists `Retry-After` backpressure and resumes incomplete work.

Primary references:

- exe.dev Login with exe: https://exe.dev/docs/login-with-exe
  - authenticated requests get `X-ExeDev-UserID` and `X-ExeDev-Email`.
- exe.dev proxy: https://exe.dev/docs/proxy
- exe.dev persistent disks: https://exe.dev/docs/serverful
- Groq speech-to-text: https://console.groq.com/docs/speech-to-text
  - File input, word/segment timestamps, and chunking guidance.
- Groq API reference: https://console.groq.com/docs/api-reference
- Groq rate limits: https://console.groq.com/docs/rate-limits
- Cloudflare R2 S3 auth: https://developers.cloudflare.com/r2/api/tokens/
- R2 presigned URLs: https://developers.cloudflare.com/r2/api/s3/presigned-urls/
- R2 pricing: https://developers.cloudflare.com/r2/pricing/
- Litestream S3-compatible/R2: https://litestream.io/guides/s3-compatible/
- Litestream replicate: https://litestream.io/reference/replicate/
- Storyteller algorithm: https://storyteller-platform.dev/docs/the-algorithm/
- tale-align forced alignment quality gates: https://github.com/samuelcole/tale-align

User-reference implementation inspected:

- https://github.com/jgbrwn/mst3k-anything
- `src/mst3k/ingest.py` uses `yt-dlp`, `--remote-components ejs:github`, no cookies, then retries YouTube with `youtube:player_client=android`.
