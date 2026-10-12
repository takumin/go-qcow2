# https://just.systems
#
# The single source of truth for formatting, linting, testing and building.
# CI runs exactly these recipes.

set shell := ['bash', '-euo', 'pipefail', '-c']

mod tool '.just/tool.just'
mod format '.just/format.just'
mod test '.just/test.just'
mod coverage '.just/coverage.just'
mod audit '.just/audit.just'
mod build '.just/build.just'

# Run the whole local pipeline
[default]
all: tool::all format::all test::all build::all
