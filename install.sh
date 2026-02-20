#!/bin/bash
set -e

INSTALL_DIR="$HOME/.local/bin"

# Check for Go
if ! command -v go &> /dev/null; then
    echo "Error: Go is not installed."
    echo "Install it from https://go.dev/dl/"
    exit 1
fi

echo "Building larva..."
cd "$(dirname "$0")"
go get github.com/BurntSushi/toml
go build -o larva larva.go

echo "Installing to $INSTALL_DIR..."
mkdir -p "$INSTALL_DIR"
mv larva "$INSTALL_DIR/larva"

# Check if install dir is on PATH
if ! echo "$PATH" | grep -q "$INSTALL_DIR"; then
    echo ""
    echo "Add this to your shell profile (~/.bashrc or ~/.zshrc):"
    echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
fi

echo "Done. Run 'larva' from any project with a larva.toml."
