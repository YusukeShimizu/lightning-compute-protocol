# proto-go

`proto-go` is a shared Go module that contains the generated protobuf/gRPC types for the LCPD API.

- Go module: `github.com/bruwbird/lcp/proto-go`
- Packages: `github.com/bruwbird/lcp/proto-go/lcpd/v1`
- Source `.proto`: currently `go-lcpd/proto/**` (eventually intended to move to repo-root `proto/**`)

## Regenerating code

From the repo root:

1) Ensure `buf` is available.
2) Run generation from `go-lcpd/` (because that is where the Buf module config lives today):

```sh
cd go-lcpd
buf generate --template ../proto-go/buf.gen.yaml
```

CI enforces that `proto-go/` is up to date via `.github/workflows/proto-go.yml`.

