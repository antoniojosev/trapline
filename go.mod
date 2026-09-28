module github.com/antoniojosev/trapline

go 1.26.1

// Pinned to a patched toolchain, not merely a recent one. govulncheck on
// go1.26.1 reported 14 reachable standard-library vulnerabilities in the code
// paths this product actually uses — crypto/tls, crypto/x509, net/http,
// net/url — and this is a service whose ingest endpoint faces the open
// internet. Raise it when a newer patch lands; `make vuln` is what tells you.
toolchain go1.26.6

require (
	github.com/klauspost/compress v1.19.2
	github.com/modelcontextprotocol/go-sdk v1.8.0
	golang.org/x/crypto v0.55.0
	modernc.org/sqlite v1.57.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
