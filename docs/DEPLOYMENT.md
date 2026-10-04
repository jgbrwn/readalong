# exe.dev + R2 deployment

## exe.dev

The app always binds to `127.0.0.1`; configure its port with `APP_PORT` (default `8000`). Configure the exe.dev HTTP proxy/share port to the same port while keeping sharing private. The application deliberately ignores any host setting so it cannot accidentally bind to `0.0.0.0`.
Set `APP_BASE_URL` to the canonical HTTPS share URL so same-origin checks remain correct if the proxy rewrites the upstream `Host` header.

exe.dev injects these authenticated headers:

- `X-ExeDev-UserID`
- `X-ExeDev-Email`

The application requires them in production. Unauthorized users receive the configured deny status (404 by default).

For local development, use `APP_ENV=development`; never enable development identity overrides in production.

The private proxy/share configuration is the access gate. Readalong does not
impose an email allowlist: any identity the proxy authenticates can receive a
new, separate account and bookshelf on first visit. Never expose the app port
directly to untrusted networks or accept identity from query parameters.

## Cover lookup contact

When the admin enables catalog cover lookup, the background worker searches
Open Library using book title/author metadata and identifies regular requests
with `Readalong/1.0`. If the owner wants a contact in the User-Agent, set
`OPEN_LIBRARY_CONTACT` in the private `.env`; this is sent to Open Library and
is not the signed-in user's email. Cover lookup does not send audio, EPUB
contents, account IDs, or source URLs.

AI cover generation uses OpenRouter's dedicated Images API directly because
the attached exe.dev LLM gateway does not expose the full image-model set.
For AI images, set `OPENROUTER_API_KEY=...` in the checkout-root `.env` and
keep that file mode `600`. If `.env` already exists from an older checkout,
add the variable manually; bootstrap only creates the file when it is absent.
`config.Load` reads `.env` from the service working directory. The regular and
Litestream systemd units unset the key before starting their process; Readalong
captures it from `.env` at startup, removes it from its own environment before
media-tool subprocesses run, and never sends or logs the value. Restart the
active service after changing `.env`.

The key is optional and **does not enable AI image generation**. It is off by
default; an administrator must open **Admin → Cover artwork**, enable image
generation, choose a model, and save. The picker offers GPT Image 2 by default,
Seedream 4.5, and FLUX.2 Pro. Image requests are paid and require OpenRouter
account credits. `scripts/doctor.sh` only reports whether a key is present; it
does not make a billable request or inspect the account balance.

Configure `OPEN_LIBRARY_CONTACT` only if you want the cover-catalog
User-Agent to include an installation contact; Readalong never substitutes a
user's exe.dev email.

## First admin

Configure `ADMIN_USER_IDS` with stable exe.dev user IDs whenever possible.
When the ID is not yet known, put the intended administrator's authenticated
email in `ADMIN_BOOTSTRAP_EMAILS` in the private `.env`. The first matching
identity claims admin access, and the database permanently binds that claim
to its `X-ExeDev-UserID`. Remove the bootstrap email afterward; the role
remains attached to that user ID. Do not put real user emails in source code
or commit `.env`.

Admin users keep their normal bookshelf. Admin tools can list accounts and
suspend/reactivate users. A separate audited operation can clone or transfer
a complete, idle book to another active account; it does not grant general
cross-account access to books, readers, or media.

## Optional Cloudflare administration

For manual Wrangler bucket administration, set `CLOUDFLARE_API_TOKEN` in
`.env` to a suitably scoped Cloudflare API token. Readalong does not use this
token, and strips it from its process environment. Do not place a bearer token
where the app or its media-tool children can inherit it.

```bash
CLOUDFLARE_API_TOKEN="$(sed -n 's/^CLOUDFLARE_API_TOKEN=//p' .env)" \
  npx wrangler r2 bucket create readalong-private
```

If the token lacks that permission, create the bucket in the dashboard or use a suitably scoped token.

## Runtime R2 credentials

R2 S3-compatible clients need distinct bucket-scoped credentials:

- `R2_ACCOUNT_ID`
- `R2_ACCESS_KEY_ID`
- `R2_SECRET_ACCESS_KEY`
- endpoint `https://<ACCOUNT_ID>.r2.cloudflarestorage.com`

Create an R2 Object Read & Write access key scoped only to the bucket where
possible. Set the account ID, Access Key ID, and Secret Access Key in the
private `.env`; use `R2_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com`.
Never commit `.env`.

## Litestream

`deploy/litestream.yml` is an optional R2/S3-compatible SQLite replica example.
Verify it against the Litestream version you install before enabling it.

Once Litestream is installed, `R2_ENABLED=true`, and the R2 S3 credentials are
configured in `.env`, run `make install-litestream-service` to replace the
regular app unit with the Litestream wrapper. The wrapper also removes
`OPENROUTER_API_KEY` from its environment; the child Readalong process reads
the private `.env` itself. To switch back, run `make install-service`, which
stops/disables the wrapper before enabling the regular unit. Alternatively, run:

```bash
litestream replicate -config ./deploy/litestream.yml \
  -restore-if-db-not-exists \
  -exec './bin/readalong'
```

Flags must precede positional arguments when using command-line mode.
Test a restore into a copied database/data directory before relying on backups.

## Asset mirror

Use rclone rather than adding an S3 SDK to the app solely for backup:

```bash
rclone sync ./data/books readalong-r2:readalong-private/readalong/books \
  --checksum --fast-list
```

Run after completed imports and/or from a systemd timer. The hot app path remains local.

## systemd

Clone the repository into `~/readalong`, configure `.env`, and run
`make install-service` to build, install, enable, and start the system-wide
`readalong.service` unit. The unit is installed under `/etc/systemd/system` but
runs as the checkout owner, using that checkout as its working directory. The
installer renders paths for the current user and checkout, stops the optional
Litestream wrapper when switching back, and keeps the app bound to loopback.

## Restore drill

1. Stop app and Litestream.
2. Move `data/app.db` aside.
3. Restore SQLite from R2 with Litestream.
4. Sync book assets back from R2 with rclone.
5. Run SQLite `PRAGMA quick_check`.
6. Start app and open a known test book.

Do this once before trusting backups.
