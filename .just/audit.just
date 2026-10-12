[private]
mod tool 'tool.just'

set shell := ['bash', '-euo', 'pipefail', '-c']
set working-directory := '..'

# The vulnerability database changes on its own, so this always runs fresh.
# It is not part of `just all` but gates CI.

# Scan for known vulnerabilities
[default]
advisories: (tool::aqua 'go') (tool::aqua 'govulncheck') tool::go-mod
    govulncheck ./...
