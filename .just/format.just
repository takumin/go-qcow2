[private]
mod tool 'tool.just'

set shell := ['bash', '-euo', 'pipefail', '-c']
set working-directory := '..'

# Run every formatter
[default]
all: gomod gofmt yamlfmt pinact justfmt

# Tidy go.mod and go.sum
gomod: (tool::aqua 'go')
    go mod tidy

# Format Go sources
gofmt: (tool::aqua 'go')
    gofmt -s -w .

# Format YAML files
yamlfmt: (tool::aqua 'yamlfmt')
    yamlfmt .

# Pin GitHub Actions to commit SHAs
pinact: (tool::aqua 'pinact')
    pinact run

# Format the justfiles
justfmt: (tool::aqua 'just')
    for f in Justfile .just/*.just; do just --unstable --fmt --justfile "$f"; done
