#!/bin/zsh
# Starts the WhatsApp bridge in this Terminal window. Opened by the "WhatsApp Bridge" app in the Dock.

export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/local/go/bin:$PATH"
cd "${0:A:h}" || exit 1

port="${WHATSAPP_MCP_PORT:-8080}"
print -Pn "\e]0;WhatsApp Bridge\a"

# Two bridges on one WhatsApp session would knock each other offline. A bridge is a
# process started from this folder: bin/bridge, or "go run ." and the program it builds.
bridge_dir="${0:A:h}"
running=""
for pid in $(pgrep -f '(^|/)bin/bridge$|^go run|/exe/whatsapp-client$'); do
  if [[ "$(lsof -a -p "$pid" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p')" == "$bridge_dir" ]]; then
    running="$pid"
    break
  fi
done
if [[ -n "$running" ]]; then
  echo "The WhatsApp bridge is already running (process $running)."
  echo "You can close this window."
  exit 0
fi

# Something else on the port would stop the bridge from starting its API
if listener="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -Fc 2>/dev/null | sed -n 's/^c//p' | head -1)" && [[ -n "$listener" ]]; then
  echo "Port $port is in use by another program ($listener), so the bridge cannot start."
  echo "Stop that program, or start the bridge and the MCP server with WHATSAPP_MCP_PORT set to a free port."
  exit 1
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
