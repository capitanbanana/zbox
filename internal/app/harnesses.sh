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
# rdx redacts identity and secrets in outgoing prompts. It is installed as a uv
# tool so its proxy can run from a stable venv under systemd.
rdx_tool="$HOME/.local/share/uv/tools/claude-code-redact"
if ! test -x "$rdx_tool/bin/rdx"; then
  if ! test -x "$HOME/.local/bin/uv"; then
    uv_installer=$(mktemp)
    trap 'rm -f "$uv_installer"' EXIT
    curl -fsSL --proto '=https' --retry 3 https://astral.sh/uv/install.sh -o "$uv_installer"
    UV_INSTALL_DIR="$HOME/.local/bin" UV_NO_MODIFY_PATH=1 sh "$uv_installer"
  fi
  # Not on PyPI yet: install the pinned upstream commit from git.
  # socksio lets httpx follow the guest ALL_PROXY (socks5h) without failing.
  "$HOME/.local/bin/uv" tool install --quiet --with socksio git+https://github.com/paroque28/claude-code-redact@d9c57fa
fi
# d9c57fa renamed the mapping cache but left four `_cache.` uses behind, which
# turns every proxied request into a 500. Keep the pin and fix the name.
rdx_server=""
for candidate in "$rdx_tool"/lib/python*/site-packages/rdx/proxy/server.py; do
  if [ -f "$candidate" ]; then rdx_server=$candidate; break; fi
done
if [ -n "$rdx_server" ] && grep -q '_cache\.' "$rdx_server"; then
  python3 -c 'import pathlib,sys; p=pathlib.Path(sys.argv[1]); p.write_text(p.read_text().replace("_cache.","cache."))' "$rdx_server"
fi
for harness in claude codex pi ori; do
  test -x "$HOME/.local/bin/$harness"
done
test -x "$rdx_tool/bin/rdx"
test -x "$rdx_tool/bin/python"
