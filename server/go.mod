module github.com/map588/memstate-mcp/server

go 1.27.1

// The release binaries are built with this Go version (setup-go reads it in
// CI). Keep it at the newest patch release: the standard library gets
// security fixes there.
toolchain go1.27.3

require (
	github.com/mattn/go-isatty v0.0.24
	golang.org/x/sys v0.47.0
	modernc.org/sqlite v1.58.0
	pgregory.net/rapid v1.3.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
