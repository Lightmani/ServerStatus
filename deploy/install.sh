#!/usr/bin/env bash
# ServerStatus installer for Debian / Ubuntu (any systemd-based Linux).
#
#   ./install.sh server [--listen :8080]
#   ./install.sh agent  --server URL --id ID --secret SECRET [--interval 3s]
#   ./install.sh uninstall [server|agent] [--purge]
#
# Run it from an extracted release directory, or pipe it from GitHub; when the
# binaries are not next to the script, the matching release is downloaded.
set -euo pipefail

REPO="${SERVERSTATUS_REPO:-Lightmani/ServerStatus}"
VERSION="${SERVERSTATUS_VERSION:-latest}"
BIN_DIR=/usr/local/bin
CONF_DIR=/etc/serverstatus
DATA_DIR=/var/lib/serverstatus
SVC_USER=serverstatus

die() { echo "error: $*" >&2; exit 1; }
info() { echo "==> $*"; }

usage() {
  sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

[ $# -ge 1 ] || usage 1
ROLE="$1"; shift
SERVER_URL="" NODE_ID="" SECRET="" INTERVAL="" LISTEN="" PURGE=0 UNINSTALL_ROLE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --server)   SERVER_URL="$2"; shift 2 ;;
    --id)       NODE_ID="$2"; shift 2 ;;
    --secret)   SECRET="$2"; shift 2 ;;
    --interval) INTERVAL="$2"; shift 2 ;;
    --listen)   LISTEN="$2"; shift 2 ;;
    --version)  VERSION="$2"; shift 2 ;;
    --purge)    PURGE=1; shift ;;
    server|agent) UNINSTALL_ROLE="$1"; shift ;;
    -h|--help)  usage 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "please run as root (sudo)"
command -v systemctl >/dev/null || die "systemd is required"

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)   echo amd64 ;;
    aarch64|arm64)  echo arm64 ;;
    armv7l|armv8l)  echo armv7 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || echo .)"
TMP=""
cleanup() { [ -n "$TMP" ] && rm -rf "$TMP"; }
trap cleanup EXIT

# locate_binary NAME -> prints path to the binary (downloading if needed)
locate_binary() {
  local name="$1"
  if [ -x "$SCRIPT_DIR/$name" ]; then echo "$SCRIPT_DIR/$name"; return; fi
  if [ -z "$TMP" ]; then
    TMP="$(mktemp -d)"
    local arch asset url
    arch="$(detect_arch)"
    asset="serverstatus_linux_${arch}.tar.gz"
    if [ "$VERSION" = latest ]; then
      url="https://github.com/${REPO}/releases/latest/download/${asset}"
    else
      url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
    fi
    info "downloading $url" >&2
    if command -v curl >/dev/null; then curl -fsSL "$url" -o "$TMP/a.tgz"
    elif command -v wget >/dev/null; then wget -qO "$TMP/a.tgz" "$url"
    else die "curl or wget is required"; fi
    tar -xzf "$TMP/a.tgz" -C "$TMP"
  fi
  local p
  p="$(find "$TMP" -type f -name "$name" | head -n1)"
  [ -n "$p" ] || die "$name not found in release archive"
  echo "$p"
}

ensure_user() {
  if ! id "$SVC_USER" >/dev/null 2>&1; then
    info "creating system user $SVC_USER"
    useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$SVC_USER"
  fi
}

install_server() {
  local bin; bin="$(locate_binary serverstatus-server)"
  ensure_user
  install -m 0755 "$bin" "$BIN_DIR/serverstatus-server"
  install -d -m 0750 -o root -g "$SVC_USER" "$CONF_DIR"
  install -d -m 0750 -o "$SVC_USER" -g "$SVC_USER" "$DATA_DIR"
  if [ ! -f "$CONF_DIR/server.yaml" ]; then
    "$BIN_DIR/serverstatus-server" config-example > "$CONF_DIR/server.yaml"
    [ -n "$LISTEN" ] && sed -i "s|^listen: .*|listen: \"$LISTEN\"|" "$CONF_DIR/server.yaml"
    chown root:"$SVC_USER" "$CONF_DIR/server.yaml"; chmod 0640 "$CONF_DIR/server.yaml"
    info "wrote $CONF_DIR/server.yaml"
  fi
  cat > /etc/systemd/system/serverstatus-server.service <<EOF
[Unit]
Description=ServerStatus server
After=network-online.target
Wants=network-online.target

[Service]
User=$SVC_USER
Group=$SVC_USER
ExecStart=$BIN_DIR/serverstatus-server -c $CONF_DIR/server.yaml serve
Restart=always
RestartSec=3
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ReadWritePaths=$DATA_DIR
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable serverstatus-server >/dev/null 2>&1
  systemctl restart serverstatus-server
  info "server installed and started: systemctl status serverstatus-server"
  cat <<EOF

Next steps:
  1. Edit $CONF_DIR/server.yaml (timezone, public_url, TLS...) then: systemctl restart serverstatus-server
  2. Add a node:        serverstatus-server -c $CONF_DIR/server.yaml node add -name "HK-1" -location "Hong Kong"
  3. (optional) Protect the dashboard: serverstatus-server -c $CONF_DIR/server.yaml passwd
EOF
}

install_agent() {
  local bin; bin="$(locate_binary serverstatus-agent)"
  ensure_user
  install -m 0755 "$bin" "$BIN_DIR/serverstatus-agent"
  install -d -m 0750 -o root -g "$SVC_USER" "$CONF_DIR"
  if [ -n "$SERVER_URL$NODE_ID$SECRET" ]; then
    [ -n "$SERVER_URL" ] && [ -n "$NODE_ID" ] && [ -n "$SECRET" ] || die "--server, --id and --secret must be given together"
    umask 027
    cat > "$CONF_DIR/agent.yaml" <<EOF
server: "$SERVER_URL"
node_id: "$NODE_ID"
secret: "$SECRET"
interval: ${INTERVAL:-3s}
# nic_exclude: regex of interfaces to ignore (default skips lo, docker, veth, bridges...)
# insecure_skip_verify: false
EOF
    chown root:"$SVC_USER" "$CONF_DIR/agent.yaml"; chmod 0640 "$CONF_DIR/agent.yaml"
    info "wrote $CONF_DIR/agent.yaml"
  elif [ ! -f "$CONF_DIR/agent.yaml" ]; then
    die "no $CONF_DIR/agent.yaml yet; pass --server, --id and --secret"
  fi
  cat > /etc/systemd/system/serverstatus-agent.service <<EOF
[Unit]
Description=ServerStatus agent
After=network-online.target
Wants=network-online.target

[Service]
User=$SVC_USER
Group=$SVC_USER
ExecStart=$BIN_DIR/serverstatus-agent -c $CONF_DIR/agent.yaml
Restart=always
RestartSec=5
# raw ICMP sockets for targeted ping monitoring (only this capability)
AmbientCapabilities=CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_RAW
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
Nice=10

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable serverstatus-agent >/dev/null 2>&1
  systemctl restart serverstatus-agent
  info "agent installed and started: journalctl -u serverstatus-agent -f"
}

uninstall() {
  local roles="${UNINSTALL_ROLE:-server agent}"
  for r in $roles; do
    if systemctl list-unit-files "serverstatus-$r.service" >/dev/null 2>&1; then
      systemctl disable --now "serverstatus-$r" >/dev/null 2>&1 || true
    fi
    rm -f "/etc/systemd/system/serverstatus-$r.service" "$BIN_DIR/serverstatus-$r"
    info "removed serverstatus-$r"
  done
  systemctl daemon-reload
  if [ "$PURGE" -eq 1 ]; then
    for r in $roles; do rm -f "$CONF_DIR/$r.yaml"; done
    [[ " $roles " == *" server "* ]] && rm -rf "$DATA_DIR"
    rmdir "$CONF_DIR" 2>/dev/null || true
    info "configuration and data purged"
  fi
}

case "$ROLE" in
  server) install_server ;;
  agent) install_agent ;;
  uninstall) uninstall ;;
  -h|--help|help) usage 0 ;;
  *) usage 1 ;;
esac
