# Validation

Run the automated checks from the repository root:

```sh
gofmt -w cmd internal
go test ./...
go vet ./...
make build
deno check --no-config --no-lock webui/static/app.js webui/static/reader.js
```

The pipeline integration tests generate short synthetic audio locally and use
a fake Groq endpoint; they do not require API credentials or consume provider
quota.

Optional live smoke tests can download media and call Groq. Use only a
public/authorized short sample, understand the account's current rate limits,
and never run those tests automatically in CI.
