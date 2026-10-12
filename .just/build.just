[private]
mod tool 'tool.just'

set shell := ['bash', '-euo', 'pipefail', '-c']
set working-directory := '..'

# GOOS/GOARCH pairs the library must compile for. Hole punching and
# fdatasync are Linux specific, so the others exercise the fallbacks.

targets := 'linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64'

# Compile (including tests) for every target
[default]
all: (tool::aqua 'go') tool::go-mod
    #!/usr/bin/env bash
    set -euo pipefail
    for t in {{ targets }}; do
        echo "build ${t}"
        export GOOS="${t%/*}" GOARCH="${t#*/}" CGO_ENABLED=0
        go build ./...
        go test -c -o /dev/null .
    done
