[private]
mod tool 'tool.just'

set shell := ['bash', '-euo', 'pipefail', '-c']
set working-directory := '..'

# Base reviewdog flags applied everywhere (local and CI). They are the lint
# gate: -filter-mode nofilter lints the whole tree (not just changed lines)
# and -fail-level any fails on any finding. CI appends its own flags through
# the REVIEWDOG_ARGS environment variable; they are added to these, never
# substituted, so the gate cannot be dropped silently.
base_args := "-filter-mode nofilter -log-level warning -fail-level any"

# Run every linter
[default]
all: editorconfig-checker biome yamllint renovate gosec golangci-lint

# Print the CI matrix of linters (every recipe in the lint group)
matrix: (tool::aqua 'reviewdog')
    @just --dump --dump-format json | jq -cM -f scripts/lint-matrix.jq

[private]
run runner: (tool::aqua 'reviewdog')
    reviewdog -runners '{{ runner }}' -reporter "${REVIEWDOG_REPORTER:-local}" {{ base_args }} ${REVIEWDOG_ARGS:-}

# Check files against .editorconfig
[group('lint')]
editorconfig-checker: (tool::aqua 'editorconfig-checker') (run 'editorconfig-checker')

# Lint JSON files
[group('lint')]
biome: (tool::aqua 'biome') (run 'biome')

# Lint YAML files
[group('lint')]
yamllint: (tool::aqua 'yamllint') (run 'yamllint')

# Not a reviewdog runner: jv reports schema violations in a free-form tree
# that no errorformat can map to a position, so it fails the recipe directly.

# Validate the Renovate configuration
[group('lint')]
renovate: (tool::aqua 'jv')
    jv https://docs.renovatebot.com/renovate-schema.json .renovaterc.json

# Run the Go security checker
[group('lint')]
gosec: (tool::aqua 'gosec') (tool::aqua 'go') tool::go-mod (run 'gosec')

# Run golangci-lint (its standard linters include go vet)
[group('lint')]
golangci-lint: (tool::aqua 'golangci-lint') (tool::aqua 'go') tool::go-mod (run 'golangci-lint')
