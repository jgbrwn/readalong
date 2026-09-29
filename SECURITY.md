# Security policy

Readalong is designed to run as a private service behind the exe.dev
authentication proxy. Source code being public does not make an installation's
bookshelf or media public.

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting for this repository when
available. Do not publish credentials, private media, or an exploitable
vulnerability in a public issue.

## Deployment requirements

- Keep the exe.dev share private and invite users through exe.dev.
- Keep the Go listener on `127.0.0.1`; do not expose the service port directly.
- Keep `.env`, SQLite databases, media, and backup credentials out of Git.
- Use a distinct, bucket-scoped R2 S3 key pair for R2; a Cloudflare API bearer
  token is not an S3 credential.
- Use `ADMIN_USER_IDS` or a one-time `ADMIN_BOOTSTRAP_EMAILS` value; never
  hard-code real identities in source.

Report suspected credential exposure promptly and rotate affected credentials.
