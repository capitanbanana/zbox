#!/bin/bash
# Install the CLI and its pinned host dependencies without Homebrew or sudo.
# Keep execution in main so a truncated curl pipe cannot run a partial script.
set -euo pipefail

fail() { printf 'agents installer: %s\n' "$*" >&2; exit 1; }
say() { printf 'agents installer: %s\n' "$*"; }
download() { curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --retry 3 --connect-timeout 20 "$1" -o "$2"; }
verify() {
  local actual
  actual=$(shasum -a 256 < "$1")
  [[ ${actual%% *} == "$2" ]] || fail "SHA-256 mismatch: ${1##*/}"
}

main() {
  local version=v0.1.0 prefix="${HOME}/.local" modify_path=1
  local lima_version=2.1.1 sing_box_version=1.13.11
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --version|--prefix)
        [[ $# -ge 2 && -n $2 ]] || fail "$1 needs a value"
        case "$1" in --version) version=$2 ;; --prefix) prefix=$2 ;; esac
        shift 2 ;;
      --no-modify-path) modify_path=0; shift ;;
      -h|--help)
        printf '%s\n' 'Usage: bash install.sh [--version vX.Y.Z] [--prefix DIR] [--no-modify-path]' \
          'Installs agents, the Linux guest helper, Lima 2.1.1 and sing-box 1.13.11.' \
          'Default prefix: ~/.local. Re-run to install the version selected by this script.'
        return ;;
      *) fail "Unknown option: $1" ;;
    esac
  done
  [[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'Version must be vX.Y.Z'
  [[ $(uname -s) == Darwin ]] || fail 'Only macOS is supported'
  [[ $(id -u) != 0 ]] || fail 'Run as your regular user, without sudo'
  local os_version os_major os_minor
  os_version=$(sw_vers -productVersion)
  IFS=. read -r os_major os_minor _ <<< "$os_version"
  (( os_major > 13 || (os_major == 13 && os_minor >= 5) )) || fail 'macOS 13.5 or newer is required'

  local arch lima_arch lima_sha sing_box_sha
  arch=$(uname -m)
  # Select native ARM binaries even from a terminal running under Rosetta.
  if [[ $arch == x86_64 ]] && [[ $(sysctl -in sysctl.proc_translated 2>/dev/null || true) == 1 ]]; then arch=arm64; fi
  case "$arch" in
    arm64)
      lima_arch=arm64
      lima_sha=b6b0e6701189cd8c4e549cc39e6d054dc681487798b9b774ad2cbd30c08b2bd8
      sing_box_sha=8fbeffbd6b737d0d3416428a126cce11002e60c89a006e42f1fbf6906802000b ;;
    x86_64)
      arch=amd64; lima_arch=x86_64
      lima_sha=2dc5b10aa3a4f26d08c1f3fe83e37e01f85a7d9db0d1d5cb6985b18af96ab07d
      sing_box_sha=09f7d509b10c499eac8d89470046d40a28f9f3e1b0fdea08202e6f2cc888f469 ;;
    *) fail "Unsupported CPU: $arch" ;;
  esac
  local command
  for command in curl tar shasum ssh awk mktemp; do
    command -v "$command" >/dev/null || fail "Missing macOS system command: $command"
  done
  [[ $prefix == /* && $prefix != *$'\n'* && $prefix != *:* ]] || fail 'Prefix must be an absolute path without newlines or colons'
  mkdir -p "$prefix/bin" "$prefix/share/agents"
  prefix=$(cd "$prefix" && pwd -P)
  [[ ! -d $prefix/bin/agents ]] || fail "$prefix/bin/agents is a directory"
  local store="$prefix/share/agents" runtime
  runtime="$store/$version-$arch-lima$lima_version-sing-box$sing_box_version"
  local lock="$store/.install-lock"
  mkdir "$lock" 2>/dev/null || fail "Another install is running (lock: $lock)"
  # Globals are limited to cleanup state: EXIT also runs after main returns.
  install_lock=$lock
  install_stage=
  install_launcher=
  trap '[[ -z $install_stage ]] || rm -rf "$install_stage"; [[ -z $install_launcher ]] || rm -f "$install_launcher"; rmdir "$install_lock"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  if [[ ! -d $runtime ]]; then
    install_stage=$(mktemp -d "$store/.install.XXXXXXXX")
    local stage=$install_stage archive="agents-$version-darwin-$arch.tar.gz" base expected
    base="https://github.com/capitanbanana/zbox/releases/download/$version"
    say "Downloading agents $version ($arch)"
    download "$base/SHA256SUMS" "$stage/SHA256SUMS"
    expected=$(awk -v file="$archive" '$2 == file {print $1}' "$stage/SHA256SUMS")
    [[ $expected =~ ^[a-f0-9]{64}$ ]] || fail "Missing or ambiguous checksum for $archive"
    download "$base/$archive" "$stage/agents.tar.gz"
    verify "$stage/agents.tar.gz" "$expected"
    mkdir "$stage/app" "$stage/lima" "$stage/sing-box"
    tar -xzf "$stage/agents.tar.gz" -C "$stage/app"

    say "Downloading Lima $lima_version and sing-box $sing_box_version"
    download "https://github.com/lima-vm/lima/releases/download/v$lima_version/lima-$lima_version-Darwin-$lima_arch.tar.gz" "$stage/lima.tar.gz"
    verify "$stage/lima.tar.gz" "$lima_sha"
    tar -xzf "$stage/lima.tar.gz" -C "$stage/lima"
    download "https://github.com/SagerNet/sing-box/releases/download/v$sing_box_version/sing-box-$sing_box_version-darwin-$arch.tar.gz" "$stage/sing-box.tar.gz"
    verify "$stage/sing-box.tar.gz" "$sing_box_sha"
    tar -xzf "$stage/sing-box.tar.gz" --strip-components=1 -C "$stage/sing-box"
    rm "$stage/agents.tar.gz" "$stage/lima.tar.gz" "$stage/sing-box.tar.gz"
    validate_runtime "$stage" "$version" "$arch" "$lima_version" "$sing_box_version"
    mv "$stage" "$runtime"
    install_stage=
  else
    say "Reusing $runtime"
    validate_runtime "$runtime" "$version" "$arch" "$lima_version" "$sing_box_version"
  fi

  # The wrapper pins dependencies for agents and its child processes only.
  # All downloads and checks finish before replacing the previous launcher.
  install_launcher=$(mktemp "$prefix/bin/.agents.XXXXXXXX")
  {
    printf '#!/bin/bash\n'
    printf 'export PATH=%q:"$PATH"\n' "$runtime/lima/bin:$runtime/sing-box"
    printf 'exec %q "$@"\n' "$runtime/app/agents"
  } > "$install_launcher"
  chmod 755 "$install_launcher"
  mv -f "$install_launcher" "$prefix/bin/agents"
  install_launcher=

  local path_line profile login_shell=${SHELL:-}
  printf -v path_line 'export PATH=%q:"$PATH"' "$prefix/bin"
  if [[ $modify_path == 1 ]]; then
    case "${login_shell##*/}" in
      zsh) add_path "${ZDOTDIR:-$HOME}/.zshrc" "$path_line" ;;
      bash)
        add_path "$HOME/.bashrc" "$path_line"
        # Bash reads only the first existing login profile.
        profile="$HOME/.bash_profile"
        if [[ ! -e $profile && -e $HOME/.bash_login ]]; then profile="$HOME/.bash_login"
        elif [[ ! -e $profile && -e $HOME/.profile ]]; then profile="$HOME/.profile"; fi
        add_path "$profile" "$path_line" ;;
      *) say 'Shell profile not changed; add the bin directory to your shell PATH.' ;;
    esac
  fi
  say "Installed $version to $prefix/bin/agents"
  printf '\nFor this terminal (or open a new one):\n  %s\n\nThen:\n  agents create\n  agents start\n  agents pi .\n' "$path_line"
}

validate_runtime() {
  local dir=$1 version=$2 arch=$3 lima_version=$4 sing_box_version=$5
  [[ -x $dir/app/agents && -x $dir/app/agents-linux-$arch && -x $dir/lima/bin/limactl && -x $dir/sing-box/sing-box ]] || fail "Incomplete installation: $dir"
  [[ $("$dir/app/agents" --version) == "agents $version" ]] || fail 'Unexpected agents version'
  [[ $("$dir/lima/bin/limactl" --version) == "limactl version $lima_version" ]] || fail 'Unexpected Lima version'
  local reported
  reported=$("$dir/sing-box/sing-box" version)
  [[ ${reported%%$'\n'*} == "sing-box version $sing_box_version" ]] || fail 'Unexpected sing-box version'
}

add_path() {
  local profile=$1 line=$2
  if [[ ! -f $profile ]] || ! grep -qxF "$line" "$profile"; then
    mkdir -p "$(dirname "$profile")"
    printf '\n# agents CLI\n%s\n' "$line" >> "$profile"
    say "Added PATH to $profile"
  fi
}

main "$@"
