[private]
mod tool 'tool.just'

set shell := ['bash', '-euo', 'pipefail', '-c']
set working-directory := '..'

# -short samples the crash simulation; `just test crash` runs all of it.

# Run the tests with the race detector
[default]
all: (tool::aqua 'go') tool::go-mod
    go test -race -short ./...

# Run the full crash-consistency simulation
crash: (tool::aqua 'go') tool::go-mod
    go test -count=1 -run TestCrashConsistency ./...
