#!/bin/sh
# ArkNet Node bootstrap installer for a fresh Ubuntu LTS machine.
#
#   curl -fsSLO <release-site>/install.sh
#   sudo sh install.sh --core-url https://<arknet-core>
#
# Safe to re-run: existing settings, data and the installed release are kept;
# a re-run re-starts and re-validates the Node. No repository clone and no
# Compose editing are needed. See docs/node-installation.md.
set -eu
ORIGINAL_ARGS="$*"

# Set when publishing a release site; --release-url overrides it.
RELEASE_URL_DEFAULT="__ARKNET_RELEASE_URL__"

SUPPORTED_UBUNTU="22.04 24.04 26.04"
MIN_MEMORY_MIB=1900   # "2 GB" machines report slightly less than 2048 MiB
MIN_DISK_GIB=20
MIN_COMPOSE="2.20.0"

RELEASE_URL=""
CORE_URL=""
CHANNEL="stable"
ARKNET_HOME="/opt/arknet"
ALLOW_HTTP="false"
INSTALL_DOCKER="true"
TOOL="/usr/local/bin/arknet-node"
# Test hooks (not for operators).
OS_RELEASE="${ARKNET_INSTALL_OS_RELEASE:-/etc/os-release}"
MEMINFO="${ARKNET_INSTALL_MEMINFO:-/proc/meminfo}"
ROOT_UID="${ARKNET_INSTALL_ROOT_UID:-0}"

step() { printf '\n==> %s\n' "$1"; }
info() { printf '    %s\n' "$1"; }
fail() {
    printf '\nINSTALL FAILED: %s\n' "$1" >&2
    shift
    for line in "$@"; do printf '  %s\n' "$line" >&2; done
    exit 1
}

usage() {
    cat <<'EOF'
Usage: sudo sh install.sh [options]

  --core-url URL       ArkNet Core this Node enrolls with (HTTPS). Without it the
                       Node runs locally and stays in PROVISIONING.
  --release-url URL    ArkNet release site (default: the site this installer came from).
  --channel CHANNEL    stable (default) or development.
  --home DIR           Installation directory (default /opt/arknet).
  --no-install-docker  Only validate Docker; never install it.
  --allow-insecure-http
                       Development/test only: allow plain-HTTP Core and release URLs.
  -h, --help           Show this help.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --core-url) [ $# -ge 2 ] || fail "--core-url needs a value."; CORE_URL="$2"; shift 2 ;;
        --release-url) [ $# -ge 2 ] || fail "--release-url needs a value."; RELEASE_URL="$2"; shift 2 ;;
        --channel) [ $# -ge 2 ] || fail "--channel needs a value."; CHANNEL="$2"; shift 2 ;;
        --home) [ $# -ge 2 ] || fail "--home needs a value."; ARKNET_HOME="$2"; shift 2 ;;
        --no-install-docker) INSTALL_DOCKER="false"; shift ;;
        --allow-insecure-http) ALLOW_HTTP="true"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) usage >&2; fail "Unknown option: $1" ;;
    esac
done

# ------------------------------------------------------------------ options
[ -n "$RELEASE_URL" ] || RELEASE_URL="$RELEASE_URL_DEFAULT"
case "$RELEASE_URL" in
    __*__) fail "This copy of install.sh does not know its ArkNet release site." \
                "Download install.sh from your ArkNet release site, or pass --release-url URL." ;;
esac
RELEASE_URL="${RELEASE_URL%/}"
case "$CHANNEL" in stable|development) ;; *) fail "--channel must be stable or development." ;; esac
case "$ARKNET_HOME" in /*) ;; *) fail "--home must be an absolute path." ;; esac

is_loopback_url() {
    case "$1" in
        http://127.*|http://localhost:*|http://localhost/*|http://localhost|http://\[::1\]*) return 0 ;;
    esac
    return 1
}
check_url() { # name value
    case "$2" in
        https://*) ;;
        http://*)
            if [ "$ALLOW_HTTP" != "true" ] && ! is_loopback_url "$2"; then
                fail "$1 must use https:// ($2)." "Plain HTTP is accepted only with --allow-insecure-http (development and testing)."
            fi ;;
        *) fail "$1 must be an http(s) URL ($2)." ;;
    esac
    case "$2" in *[[:space:]\"\'\\\$\`]*) fail "$1 contains characters that are not allowed." ;; esac
}
check_url "--release-url" "$RELEASE_URL"
if [ -n "$CORE_URL" ]; then
    check_url "--core-url" "$CORE_URL"
    CORE_URL="${CORE_URL%/}"
    rest="${CORE_URL#*://}"
    case "$rest" in */*|*\?*|*\#*|*@*) fail "--core-url must be a base URL like https://core.example.net (no path, query or credentials)." ;; esac
    if is_loopback_url "$CORE_URL"; then
        fail "--core-url points at this machine's loopback address ($CORE_URL)." \
             "The Node Agent runs in a container and cannot reach Core there; use Core's network address."
    fi
fi

# ---------------------------------------------------------- prerequisites
step "Checking this machine"
[ "$(id -u)" = "$ROOT_UID" ] || fail "The installer must run as root." "Run: sudo sh install.sh $ORIGINAL_ARGS"

[ -r "$OS_RELEASE" ] || fail "Cannot read $OS_RELEASE; ArkNet Node supports Ubuntu LTS ($SUPPORTED_UBUNTU)."
# shellcheck disable=SC1090 # os-release is a root-owned system file
os_id=$(. "$OS_RELEASE" && printf '%s' "${ID:-}")
# shellcheck disable=SC1090
os_version=$(. "$OS_RELEASE" && printf '%s' "${VERSION_ID:-}")
# shellcheck disable=SC1090
os_name=$(. "$OS_RELEASE" && printf '%s' "${PRETTY_NAME:-$os_id $os_version}")
supported="false"
if [ "$os_id" = "ubuntu" ]; then
    for v in $SUPPORTED_UBUNTU; do [ "$os_version" = "$v" ] && supported="true"; done
fi
[ "$supported" = "true" ] || fail "Unsupported operating system: $os_name." \
    "ArkNet Node supports Ubuntu Server LTS $SUPPORTED_UBUNTU. Install a supported release and re-run."
info "OS: $os_name"

arch=$(dpkg --print-architecture 2>/dev/null || uname -m)
case "$arch" in
    amd64|x86_64) arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *) fail "Unsupported CPU architecture: $arch." "ArkNet Node images are published for amd64 (x86-64) and arm64 (AArch64)." ;;
esac
info "Architecture: $arch"

mem_kib=$(awk '/^MemTotal:/ {print $2}' "$MEMINFO" 2>/dev/null || true)
[ -n "$mem_kib" ] || fail "Cannot determine installed memory from $MEMINFO."
mem_mib=$((mem_kib / 1024))
[ "$mem_mib" -ge "$MIN_MEMORY_MIB" ] || fail "Not enough memory: ${mem_mib} MiB installed." \
    "ArkNet Node needs at least 2 GB of RAM (PostgreSQL, Jellyfin, Agent and UI). Add memory and re-run."
info "Memory: ${mem_mib} MiB"

disk_path="$ARKNET_HOME"
while [ ! -d "$disk_path" ]; do disk_path=$(dirname "$disk_path"); done
for path in "$disk_path" /var/lib; do
    free_kib=$(df -Pk "$path" | awk 'NR==2 {print $4}')
    free_gib=$((free_kib / 1024 / 1024))
    [ "$free_gib" -ge "$MIN_DISK_GIB" ] || fail "Not enough free disk space on $path: ${free_gib} GiB." \
        "ArkNet Node needs at least ${MIN_DISK_GIB} GiB free for images and data (Docker stores images under /var/lib/docker)."
done
info "Free disk: ${free_gib} GiB"

apt_install() {
    info "Installing $* from the Ubuntu archive"
    export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1  # no prompts or restart reports
    apt-get update -q >/dev/null || fail "apt-get update failed." "Check this machine's internet access and APT sources."
    apt-get install -y -q "$@" >/dev/null \
        || fail "Could not install: $*." "Check internet access; on Ubuntu the packages are in the 'universe' component (sudo add-apt-repository universe)."
}

missing=""
command -v curl >/dev/null 2>&1 || missing="$missing curl"
command -v python3 >/dev/null 2>&1 || missing="$missing python3"
# arknet-node creates the management UI's self-signed HTTPS certificate with openssl.
command -v openssl >/dev/null 2>&1 || missing="$missing openssl"
# shellcheck disable=SC2086 # intentional word splitting of package names
[ -z "$missing" ] || apt_install $missing
python3 -c 'import sys; sys.exit(sys.version_info < (3, 8))' || fail "Python 3.8 or newer is required for arknet-node."

# ------------------------------------------------------------------ docker
step "Checking Docker"
if ! command -v docker >/dev/null 2>&1; then
    [ "$INSTALL_DOCKER" = "true" ] || fail "Docker is not installed." \
        "Install Docker Engine with the Compose plugin, or re-run without --no-install-docker."
    apt_install docker.io docker-compose-v2
fi
if ! docker info >/dev/null 2>&1; then
    command -v systemctl >/dev/null 2>&1 && systemctl enable --now docker >/dev/null 2>&1 || true
    docker info >/dev/null 2>&1 || fail "Docker is installed but its daemon is not running." \
        "Start it with: sudo systemctl enable --now docker   (then re-run the installer)"
fi
compose_version=$(docker compose version --short 2>/dev/null | sed 's/^v//') || compose_version=""
if [ -z "$compose_version" ] && [ "$INSTALL_DOCKER" = "true" ]; then
    apt_install docker-compose-v2
    compose_version=$(docker compose version --short 2>/dev/null | sed 's/^v//') || compose_version=""
fi
[ -n "$compose_version" ] || fail "The Docker Compose plugin (docker compose) is not available." \
    "Install it with: sudo apt-get install docker-compose-v2"
lowest=$(printf '%s\n%s\n' "$MIN_COMPOSE" "$compose_version" | sort -V | head -n1)
[ "$lowest" = "$MIN_COMPOSE" ] || fail "Docker Compose $compose_version is too old; $MIN_COMPOSE or newer is required." \
    "Install a current Docker Engine and Compose plugin (https://docs.docker.com/engine/install/ubuntu/) and re-run."
info "Docker $(docker version --format '{{.Server.Version}}' 2>/dev/null), Compose $compose_version"

# ------------------------------------------------------------- directories
step "Preparing $ARKNET_HOME"
# 0755: the Agent container (UID 10001) reads releases/<version>/release.json.
install -d -m 0755 "$ARKNET_HOME" "$ARKNET_HOME/releases"
install -d -m 0700 "$ARKNET_HOME/backups"
env_file="$ARKNET_HOME/.env"
if [ -f "$env_file" ]; then
    chmod 0600 "$env_file"
    info "Keeping existing settings in $env_file"
    existing_core=$(sed -n 's/^ARKNET_CORE_URL=//p' "$env_file" | tail -n1)
    if [ -n "$CORE_URL" ] && [ "$CORE_URL" != "$existing_core" ]; then
        info "NOTE: --core-url differs from the installed ARKNET_CORE_URL ($existing_core); the installed value is kept."
    fi
else
    password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
    [ ${#password} -eq 48 ] || fail "Could not generate a database password."
    old_umask=$(umask)
    umask 077
    {
        echo "# ArkNet Node settings, created by install.sh. Keep this file private."
        echo "POSTGRES_PASSWORD=$password"
        echo "ARKNET_CORE_URL=$CORE_URL"
        [ "$ALLOW_HTTP" = "true" ] && echo "ARKNET_CORE_ALLOW_HTTP=true"
        echo "ARKNET_RELEASE_INDEX_URL=$RELEASE_URL/index.json"
        [ "$ALLOW_HTTP" = "true" ] && echo "ARKNET_RELEASE_INDEX_ALLOW_HTTP=true"
    } > "$env_file.tmp"
    umask "$old_umask"
    mv "$env_file.tmp" "$env_file"
    info "Created $env_file (private, mode 0600)"
fi

# --------------------------------------------------------------- the tool
step "Installing arknet-node"
proto="=https"
if [ "$ALLOW_HTTP" = "true" ] || is_loopback_url "$RELEASE_URL"; then
    proto="=https,http"
fi
tmp_tool=$(mktemp)
trap 'rm -f "$tmp_tool"' EXIT
curl -fsS --proto "$proto" --max-time 60 -o "$tmp_tool" "$RELEASE_URL/arknet-node" \
    || fail "Could not download arknet-node from $RELEASE_URL." "Check this machine's internet access and the release URL."
# Syntax-only check (writes no bytecode files).
if ! head -n1 "$tmp_tool" | grep -q '^#!/usr/bin/env python3' \
    || ! python3 -c 'import ast, sys; ast.parse(open(sys.argv[1]).read())' "$tmp_tool" 2>/dev/null; then
    fail "The downloaded arknet-node is not a valid ArkNet tool." "The release site may be misconfigured; do not continue."
fi
install -m 0755 "$tmp_tool" "$TOOL"
info "$TOOL"

# ------------------------------------------------------------ the release
step "Installing the ArkNet Node release"
set +e
"$TOOL" --home "$ARKNET_HOME" install --index "$RELEASE_URL/index.json" --channel "$CHANNEL"
code=$?
set -e
if [ $code -ne 0 ]; then
    printf '\nThe installer stopped; see the message above. Re-running it is safe.\n' >&2
    exit $code
fi
printf '\nNext: open the management UI shown above, check the certificate fingerprint, and create the first\n'
printf 'operator with the setup code (a new one: sudo arknet-node operator setup-code). Then approve this\n'
printf 'Node in ArkNet Core (if a Core URL was given).\n'
printf 'Manage releases later with: sudo arknet-node status | upgrade | rollback\n'
printf 'Operator access: sudo arknet-node operator setup-code | recover NAME | list | disable NAME\n'
