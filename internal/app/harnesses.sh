#!/bin/bash
set -euo pipefail
mkdir -p "$HOME/.local/bin" "$HOME/dev"
export PATH="$HOME/.local/bin:$PATH"
npm config set prefix "$HOME/.local"
if ! test -x "$HOME/.local/bin/codex"; then npm install -g @openai/codex; fi
if ! test -x "$HOME/.local/bin/claude"; then npm install -g @anthropic-ai/claude-code; fi
if ! test -x "$HOME/.local/bin/pi"; then
  node -e 'const [a,b]=process.versions.node.split(".").map(Number); if(a<22 || (a===22 && b<19)){console.error("Pi requires Node.js 22.19 or newer; update Node inside the VM.");process.exit(1)}'
  npm install -g --ignore-scripts @earendil-works/pi-coding-agent
fi
if ! test -x "$HOME/.local/bin/ori"; then
  installer=$(mktemp)
  trap 'rm -f "$installer"' EXIT
  curl -fsSL --proto '=https' --retry 3 https://openrouter.ai/labs/ori/install.sh -o "$installer"
  ORI_INSTALL_DIR="$HOME/.local/bin" bash "$installer"
fi
for harness in claude codex pi ori; do
  test -x "$HOME/.local/bin/$harness"
done
