# zbox

> **macOS-only.** macOS 13.5+ (Apple Silicon or Intel). Linux host support is unavailable.

A Go CLI that runs Claude Code, Codex, Pi, and OpenRouter Ori inside a Linux VM on a
Mac. Your terminal and host applications stay on macOS; selected folders are exported to
the VM and selected domains are routed through the Mac's network or corporate VPN.
Flows, ports, and boundaries: [docs/architecture.md](docs/architecture.md).

![Architecture: terminal and host applications on the Mac, agents in the VM, explicit folder and network access](docs/diagrams/architecture.svg)

## Installation

```sh
curl -fsSL https://raw.githubusercontent.com/capitanbanana/zbox/main/install.sh | bash
zbox --version
```

Installs the CLI, its matching Linux guest helper, **Lima 2.1.1**, and **sing-box
1.13.11** with SHA-256 verification and adds its directory to zsh/bash startup files; no
Go, Homebrew, Git, or sudo needed, and other shells need PATH set manually. Re-running
updates in place and preserves VM state, mounts, credentials, and network settings. It
never creates or boots a VM. Pin with `--prefix`, `--version v0.2.0`, `--no-modify-path`.

From source (Go 1.25+, Python 3, Lima; **VLESS requires Lima 2.1.x**):

```sh
brew install lima go
make test && make install     # zbox for the Mac, zbox-linux-<arch> for the VM
make release VERSION=vX.Y.Z   # see docs/releasing.md
```

`ZBOX_GUEST_BINARY` locates the guest helper, `ZBOX_SING_BOX` a `sing-box`. Linux
binaries in release archives are guest helpers; a Linux host needs a non-VZ Lima
backend, mount sharing, an installer, and trusted-network detection.

## Quick start

```sh
zbox create                     # 4 CPUs, 8 GiB RAM, 60 GiB disk; initially stopped
zbox mounts add "$PROJECT_DIR"  # Export the selected project read/write
zbox network add '*.example.com' example.com
# zbox vpn add --stdin < "$VLESS_FILE"   # Optional: protect networking before first boot

zbox start                      # Installs packages and agent CLIs on first boot

cd "$PROJECT_DIR"
zbox claude .
zbox codex . --model MODEL
zbox codex . login --device-auth
zbox pi .
zbox ori . pi                   # Pi through OpenRouter
zbox openrouter . claude        # Alias for zbox ori . claude

zbox status && zbox stop
zbox delete                     # VM, guest auth/history, config; host folders stay
```

Authenticate **inside the VM**, separately per CLI: host agent configuration, SSH
configuration, credentials, and environment variables are never copied in, and automatic
guest port forwarding (including OAuth callbacks) is disabled.

The first `start` installs git, ripgrep, tmux, jq, Python/venv, build-essential,
checksum-verified Node 22, Claude/Codex/Pi via npm, and Ori via its installer, so it needs
internet and can take minutes; an interrupted run continues in the background and the
next command waits for it. Later starts add missing tools without recreating the disk or
restarting sessions and never upgrade existing ones:

```sh
zbox shell npm install -g @openai/codex @anthropic-ai/claude-code
zbox shell npm install -g --ignore-scripts @earendil-works/pi-coding-agent
zbox shell ori update
```

`zbox ssh` and `zbox socks` skip that installer, so they work when a download fails;
`zbox restart` stops and starts the VM. State lives in a private per-user directory with a dedicated `LIMA_HOME`, and global
flags go **before** the command:

```sh
zbox --vm work create --cpus 6 --memory 12 --disk 100   # CPUs / GiB RAM / GiB disk
zbox --vm work codex .
zbox --state-dir "$STATE_DIR" status                    # or ZBOX_HOME
```

Size flags are optional, follow `create`, and never resize an existing VM (minimums 1
CPU, 2 GiB, 10 GiB). Each profile has its own mounts, network/MCP settings, credentials,
and disk, shared by every harness in that VM. Legacy `AGENTS_*` overrides still work;
`ZBOX_*` wins.

## Sessions, folders, and harnesses

```sh
zbox ssh                                   # alias: zbox shell, interactive Bash in the VM
zbox claude . --resume                     # first argument is a host folder, mounted if new
zbox codex "$PROJECT_DIR" exec 'run the tests'
zbox pi . --provider openrouter --model PROVIDER/MODEL
zbox ori . claude|codex|pi --model PROVIDER/MODEL

zbox mounts list
zbox mounts add "$HOST_DIR" "$GUEST_DIR" --ro
zbox mounts remove "$HOST_DIR"
zbox mount "$HOST_DIR"                     # alias for mounts add; no path means current directory
```

The wrapper mounts the folder, starts the VM, and runs the agent there: one optional `--`
after the folder is removed, the rest is forwarded without local shell evaluation, and
the exit code is propagated to a TTY. New sessions never restart the proxy or other
sessions, `zbox stop` ends all VM processes, and there is no automatic tmux or reconnect
(`zbox shell tmux new -A -s work` covers long runs).

Only designated guest mount roots are accepted; overlapping and duplicate destinations
are rejected, and a subdirectory of an exported folder reuses that mount. **Changing
virtiofs mounts restarts a running VM** after printing a notice, ending **all** sessions;
exports persist until removed. RW writes change host files, an RO mount still exposes
every readable file including hidden ones, and the wrapper's state directory cannot be
exported. Pi uses a managed `PI_CODING_AGENT_DIR` whose `mcp.json` is generated from
`zbox mcp`; Ori gets a separate managed `CODEX_HOME` because it rewrites Codex auth,
while plain `zbox codex` keeps its own.

## Selected routes through the Mac

```sh
zbox network                  # network> + *.example.com / - git.corp.example / list
zbox network add '*.example.com' example.com git.corp.example
zbox network remove git.corp.example
```

The list controls **split routing**: matching hostnames go guest proxy → SSH tunnel → Mac
SOCKS5 → Mac network/VPN, everything else uses VLESS, the trusted-network bypass, or
ordinary Lima networking, and matching names resolve on the Mac. `*.example.com` matches
subdomains at any depth but not `example.com`; rules take hostnames and IPs, not URLs,
ports, or globs, and an allowed name grants **all TCP ports** to new connections only.
Programs that ignore the injected `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY` variables use VM
networking — still subject to the VLESS gateway, but outside `network` exceptions. Failed
policy retrieval is an error, never a direct fallback; host-routed connections are logged
by hostname and port only.

**Without `zbox vpn`**, ordinary Lima networking can inherit host VPN routes and reach
services on the Mac, and Lima 2.1 user networking provides host-loopback access: the
wrapper then does **not** block those paths or promise that listed names are the only
reachable addresses.

`zbox socks` (default `127.0.0.1:1080`) tunnels to **the same guest SOCKS5 proxy agents
use**, with the same split routing, DNS, and egress. Point the browser at it with DNS
through SOCKS; macOS system proxy settings, cookies, and profiles stay separate from
agents'. It is TCP CONNECT only — website access, not whole-browser isolation — and its
passwordless listener is reachable by Mac processes but not the LAN. Chrome bypasses
loopback, so guest `localhost` needs `--proxy-server='socks5://127.0.0.1:1080'
--proxy-bypass-list='<-loopback>' --host-resolver-rules='MAP * ~NOTFOUND, EXCLUDE
127.0.0.1'`.

## VLESS for the whole VM and a kill switch

```sh
brew install sing-box
zbox create
zbox vpn add --stdin < "$VLESS_FILE"       # or: zbox vpn add 'vless://…'
zbox start && zbox vpn status

zbox stop && zbox vpn update --stdin < "$VLESS_FILE" && zbox start   # or --restart on a running VM
zbox vpn delete                            # Intentionally restore ordinary networking

zbox vpn bypass current     # Show the Mac's network; no VM config required
zbox vpn bypass add home    # Trust this network, keeping VLESS elsewhere
zbox vpn bypass list
zbox vpn bypass delete home # Require VLESS at home again
```

`--stdin` keeps the secret out of shell history and arguments; the profile is stored
`0600` and status omits UUIDs and keys. Supported: TCP/raw, WebSocket, gRPC, HTTP, and
HTTPUpgrade with TLS or REALITY, including `sni`, `fp`, `pbk`, `sid`, `alpn`,
`serviceName`, and Vision; unsupported parameters such as XHTTP are rejected and TLS
verification is never disabled. Mac VPN clients and router settings are untouched.

A VPN-enabled VM has **one interface** on a dedicated Mac gateway that starts **before**
the VM boots, replacing Lima's unrestricted interface. TCP/UDP goes only through the
local VLESS SOCKS endpoint — no direct outbound, no fallback — covering provisioning,
`sudo`, and programs that ignore proxy variables. Guest DNS is DoH via Cloudflare then
Google over VLESS with no system fallback, arbitrary UDP also uses VLESS, and IPv6 and
external ICMP are blocked, so `ping` proves nothing. A dead client, broken tunnel, or
failed gateway blocks external traffic; `vpn status` reports process state, not remote
reachability, and recovery after a dead client is `zbox stop` then `zbox start`. The
gateway also symlinks that network's `leases.json` to `/dev/null` so Lima's ordinary
usernet cannot return after a failure — do not change it, and expect `limactl` without
the gateway to fail with `failed to start usernet … EOF`. Add VPN after `create` and
before the first `start` for protection from boot; partial failures keep the intent.

Bypass rules live in the VM configuration's `vpn_bypass`, survive `vpn update`, are
inactive without VPN, and update a running gateway without a restart. A match requires
the IPv4 subnet, gateway IP, **and** gateway MAC (`192.168.x.0/24` alone is not enough)
and is checked before the VM attaches; on a match, TCP/UDP and guest DNS use the Mac's
network without the extra VLESS tunnel while a router VPN still applies, IPv6 and
external ICMP stay blocked, and the protected interface remains the VM's only one.
Matching is not cryptographic authentication and can be spoofed, so do not add bypass
rules if you require an unconditional ban on direct packets during roaming.

## MCP servers

```sh
zbox mcp add SERVER_NAME -- SERVER_COMMAND ARGUMENTS     # stdio, runs in the VM
zbox mcp add SERVER_NAME --url "$REMOTE_MCP_URL"        # Remote Streamable HTTP
zbox mcp add SERVER_NAME --url "$HOST_MCP_URL" --host   # Existing server on the Mac
zbox mcp list|enable|disable|remove SERVER_NAME
```

`--host` tunnels the specified loopback TCP port on the Mac, granting **the whole port**,
not one path or tool set, and the wrapper neither installs nor starts that server.
HTTP(S) and stdio are supported; legacy SSE, arbitrary headers, and host credential
import are not, and stdio programs must be installed inside the VM. Claude starts with
`--strict-mcp-config` and generated JSON; Codex uses a managed `CODEX_HOME` with a
generated `config.toml` (other settings via `-c key=value`), so stale MCP entries cannot
survive table merges. Project settings, plugins, and agent actions can still add
capabilities inside the VM — this registry is not an enforcement mechanism. An MCP tool
that runs code, or writable files consumed by a host application, executes with that
application's permissions: the VM does not contain that execution.

## Identity and redaction

The VM presents a fictional identity ("legend") instead of the Mac's, generated once at
`zbox create` from a random seed stored in the VM configuration, so it is stable across
restarts. It supplies the guest username and home (`/home/<username>`), full name,
e-mail, GitHub handle, hostname, time zone, and the IDs written once into
`~/.claude.json` before Claude Code ever runs; that file is never rewritten afterwards.

```sh
jq -r .persona ~/.zbox/vms/default/config.json
zbox redact show

zbox redact audit --out /tmp/audit.md   # instructions + machine facts + docs
# paste into any LLM, save its JSON reply, then
zbox redact apply --file findings.json  # table, confirmation, rules
zbox redact discover [--add]            # rdx discover inside the VM
zbox start                              # regenerates .redaction_rules per mount
```

Replacements are deterministic, so the same value always maps to the same legend value
and prompt caching keeps working; already-neutral values and public domains used by
tooling are left alone. Suspected credentials found by `redact audit` are never stored
in clear text — they become hashed rules that match without keeping the secret on disk,
and `redact audit` lists only their locations. `redact discover` prints values no rule
covers yet and sends them nowhere; adding them is the explicit opt-in `--add` (or
`redact rules --new FILE --add` for a findings file), which stores exactly those values
with legend replacements and regenerates `.redaction_rules`. Values reported by `rdx
discover` are matched by literal pattern, which is how rdx itself stores them.

### The rdx proxy

Prompt redaction is done by [`rdx`](https://github.com/paroque28/claude-code-redact)
inside the guest, pinned to upstream commit `d9c57fa` and installed as a `uv` tool in
`~/.local/share/uv/tools/claude-code-redact` so it runs from a stable venv. It runs as
the user service `rdx-proxy.service` on `127.0.0.1:18642` and reaches the provider through
the ordinary guest proxy. Two local patches are applied at install: `--with socksio`, so
`httpx` can follow the guest `socks5h` proxy, and a rewrite of four `_cache.` references
left behind by an upstream rename, which otherwise turns every proxied request into a
500. `rdx setup` is deliberately not used — it would rewrite project settings and
`CLAUDE.md`.

Rules come from one generated `.redaction_rules` per project, written on the Mac side of
the mount so read-only folders work and kept out of git through `.git/info/exclude`
(entry `/.redaction_rules`).

**Coverage is limited to the Anthropic Messages API.** `rdx` serves only
`POST /v1/messages` and `/v1/messages/count_tokens`, always forwarding to
`RDX_UPSTREAM_URL` (default `https://api.anthropic.com`) with that fixed path, and it
forwards only `authorization`, `anthropic-version`, `anthropic-beta`, and `content-type`.
It is wired in via `ANTHROPIC_BASE_URL` in `~/.config/agents/proxy.env`:

| Harness | Transport | Redacted |
| --- | --- | --- |
| Claude Code | Anthropic Messages | Yes |
| Codex | OpenAI `/v1/responses` | No |
| Ori / OpenRouter | `/api/v1/chat/completions` | No |
| Pi with a non-Anthropic provider | provider-native | No |

Two more constraints matter before extending it. The body walker only understands
Anthropic content blocks (`text`, `tool_use`, `tool_result`, and a `system` string or
block list), and project selection matches `Primary working directory: …` in the system
prompt — a Claude Code-specific string. When that does not match, `rdx` falls back to
`RDX_RULES_DIR` or its own cwd, and without a `.redaction_rules` file there it forwards
requests **completely unredacted**. Covering other harnesses therefore needs path and
host passthrough, a format-agnostic JSON walker, generic SSE un-redaction, and a
project-selection mechanism independent of Claude Code's prompt text.

## Isolation and verification

There is no automatic home mount, SSH-agent forwarding, public-key import, host
authentication copy, arbitrary guest port forwarding, or Docker socket export; state
mutations use a file lock and atomic `0600` replacement. The guest is trusted only within
the VM — its Lima user has `sudo` and agents can change guest settings — so the filesystem
boundary is the VM plus the explicit export list, not a prompt, `AGENTS.md`, or a guest
firewall. See the [access table](docs/architecture.md#access-boundaries).

```sh
make test     # race detector, loopback unit/integration tests, go vet
make build && ./bin/zbox help
```

Tests cover wildcard boundaries, mount validation, argument quoting, locks, live policy
updates, SOCKS and HTTP-proxy behavior, live routing changes, and the absence of direct
fallback; VPN tests cover VLESS parsing, gateway TCP/UDP, DNS failure, intent
preservation, extra-NIC rejection, and the Lima guard. End-to-end scripts each use a
throwaway VM: [`scripts/smoke.sh`](scripts/smoke.sh), `scripts/smoke-socks.py`,
`scripts/smoke-harnesses.py`, and `scripts/smoke-redact.py` (mock upstream; proves the
host identity, internal hostname, and a hashed secret never reach it and that repeated
requests are byte-identical). Last verified on macOS arm64 with Lima 2.1.1, Claude Code
2.1.325, Codex 0.169.0, Pi 1.5.2, and Ori 0.17.0; physical network roaming was untested.
Diagrams are checked-in SVGs with Mermaid sources — see
[updating the diagrams](docs/architecture.md#updating-the-diagrams).

### Upstream references

- [Lima mounts](https://lima-vm.io/docs/config/mount/) and [networking](https://lima-vm.io/docs/config/network/user/)
- [Claude network configuration](https://code.claude.com/docs/en/network-config) and [MCP](https://code.claude.com/docs/en/mcp)
- [Pi](https://github.com/earendil-works/pi/tree/main/packages/coding-agent), [Pi MCP](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/mcp.md), [Ori](https://openrouter.ai/docs/guides/ori/harness)
