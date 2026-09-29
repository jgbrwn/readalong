# Contributing

Read `AGENTS.md` and the architecture/sync/API documents in `docs/` before
changing product behavior.

## Local checks

```sh
gofmt -w cmd internal
go test ./...
go vet ./...
make build
```

The frontend is plain embedded HTML/CSS/JavaScript; no package manager or
build step is required. Use a development identity only with
`APP_ENV=development`.

## Secrets and test data

- Never commit `.env`, tokens, databases, audio, or generated media.
- Keep test audio synthetic, public-domain, or explicitly authorized.
- Do not include user emails or private VM details in source or examples.
- Do not weaken localhost binding, proxy identity checks, owner checks, or URL
  SSRF protections to make a test easier.

Keep changes focused and update the docs/tests when behavior changes.
