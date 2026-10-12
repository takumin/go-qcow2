[private]
mod tool 'tool.just'

set shell := ['bash', '-euo', 'pipefail', '-c']
set working-directory := '..'

# Informational only: there is no threshold gate.

# Write a coverage report to coverage/
[default]
all: (tool::aqua 'go') tool::go-mod
    mkdir -p coverage
    go test -race -short -covermode atomic -coverprofile coverage/coverage.out ./...
    go tool cover -html coverage/coverage.out -o coverage/coverage.html
