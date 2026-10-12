set shell := ['bash', '-euo', 'pipefail', '-c']
set working-directory := '..'

# Install every aqua package
[default]
all:
    aqua install -a

# Install the aqua packages tagged with TAG
aqua tag:
    aqua install -t {{ tag }}

# Download and verify the Go module dependencies
go-mod: (aqua 'go')
    go mod download
    go mod verify
