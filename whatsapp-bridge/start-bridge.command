#!/bin/zsh
# Starts the WhatsApp bridge in this Terminal window. Opened by the "WhatsApp Bridge" app in the Dock.

export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/local/go/bin:$PATH"
cd "${0:A:h}" || exit 1

port="${WHATSAPP_MCP_PORT:-8080}"
print -Pn "\e]0;WhatsApp Bridge\a"

# Two bridges on one WhatsApp session would knock each other offline
if pgrep -qf "${0:A:h}/bin/bridge" || pgrep -qf "go run \.$" || lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "The WhatsApp bridge is already running (something is listening on port $port)."
  echo "You can close this window."
  exit 0
fi

# Rebuild when the program is missing or the source has changed since it was built
binary="bin/bridge"
if [[ ! -x "$binary" || -n "$(find . -maxdepth 1 \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$binary" 2>/dev/null)" ]]; then
  if command -v go >/dev/null; then
    echo "Building the bridge..."
    mkdir -p bin && go build -o "$binary" . || { echo "Build failed."; [[ -x "$binary" ]] || exit 1; echo "Starting the previous build instead."; }
  elif [[ ! -x "$binary" ]]; then
    echo "Go is not installed, so the bridge cannot be built. Install it with: brew install go"
    exit 1
  fi
fi

echo "Starting the WhatsApp bridge. Keep this window open; press Ctrl+C to stop it."
echo
exec "$binary"
