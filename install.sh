#!/bin/sh
# TopoLight installer for Linux (amd64 / arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/nizartuanku/topolight/main/install.sh | sudo sh
#
# What it does, in order — nothing else:
#   1. downloads the release tarball for this CPU from GitHub Releases and
#      verifies it against SHA256SUMS
#   2. installs /usr/local/bin/topolight and a `topolight` system user
#   3. grants the binary cap_net_raw (ICMP) and cap_net_bind_service (514/162)
#      so it never runs as root
#   4. writes /etc/topolight/topolight.env and a systemd unit, enables and
#      starts it
# Re-running upgrades the binary and keeps your data in /var/lib/topolight.
# Set TOPOLIGHT_VERSION=vX.Y.Z to pin a version; TOPOLIGHT_TARBALL=path to
# install from a local file (offline hosts).
set -eu

VERSION="${TOPOLIGHT_VERSION:-v0.1.0}"
REPO="nizartuanku/topolight"
BIN_DIR="/usr/local/bin"
DATA_DIR="/var/lib/topolight"
CONF_DIR="/etc/topolight"

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo sh install.sh)"
command -v systemctl >/dev/null 2>&1 || die "systemd not found — download the tarball and run ./topolight by hand (see docs/INSTALL.md)"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported CPU $(uname -m) — build from source with Go 1.24" ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"

if [ -n "${TOPOLIGHT_TARBALL:-}" ]; then
  say "Installing from $TOPOLIGHT_TARBALL"
  cp "$TOPOLIGHT_TARBALL" topolight.tar.gz
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
  FILE="topolight_${VERSION#v}_linux_${ARCH}.tar.gz"
  say "Downloading $FILE"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o topolight.tar.gz "$BASE/$FILE"
    curl -fsSL -o SHA256SUMS "$BASE/SHA256SUMS"
  else
    wget -qO topolight.tar.gz "$BASE/$FILE"
    wget -qO SHA256SUMS "$BASE/SHA256SUMS"
  fi
  say "Verifying checksum"
  WANT="$(grep " $FILE\$" SHA256SUMS | awk '{print $1}')"
  [ -n "$WANT" ] || die "$FILE not listed in SHA256SUMS"
  GOT="$(sha256sum topolight.tar.gz | awk '{print $1}')"
  [ "$WANT" = "$GOT" ] || die "checksum mismatch: expected $WANT got $GOT"
fi

tar -xzf topolight.tar.gz
BIN="$(find . -type f -name topolight | head -n1)"
[ -n "$BIN" ] || die "binary not found in tarball"

say "Installing $BIN_DIR/topolight"
install -m 0755 "$BIN" "$BIN_DIR/topolight.new"
mv -f "$BIN_DIR/topolight.new" "$BIN_DIR/topolight"

if ! id topolight >/dev/null 2>&1; then
  say "Creating system user topolight"
  useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin topolight 2>/dev/null || \
  adduser --system --home "$DATA_DIR" --no-create-home --shell /usr/sbin/nologin topolight
fi
mkdir -p "$DATA_DIR" "$CONF_DIR"
chown topolight:topolight "$DATA_DIR"
chmod 0700 "$DATA_DIR"

if command -v setcap >/dev/null 2>&1; then
  say "Granting cap_net_raw (ICMP) and cap_net_bind_service (514/162)"
  setcap 'cap_net_raw,cap_net_bind_service=+ep' "$BIN_DIR/topolight" || say "setcap failed — ICMP and ports <1024 need root or a sysctl; see docs/INSTALL.md"
else
  say "setcap not available — install libcap2-bin (Debian) / libcap (RHEL) for ICMP and ports <1024"
fi

if [ ! -f "$CONF_DIR/topolight.env" ]; then
  say "Writing $CONF_DIR/topolight.env"
  cat > "$CONF_DIR/topolight.env" <<EOF
# Command-line flags for TopoLight (one line; see topolight -h).
# The console listens on every interface by default here because a
# monitoring server is normally reached from a workstation. Put it behind
# HTTPS (-tls-cert/-tls-key or a reverse proxy) before exposing it widely.
TOPOLIGHT_OPTS=-listen 0.0.0.0:8432 -data $DATA_DIR -syslog-listen :514 -trap-listen :162
# Licence key (Pro/Team) — or paste it in Admin → Licence:
#TOPOLIGHT_LICENSE=SNTL1-...
EOF
  chmod 0640 "$CONF_DIR/topolight.env"
  chown root:topolight "$CONF_DIR/topolight.env"
fi

say "Writing systemd unit"
cat > /etc/systemd/system/topolight.service <<'EOF'
[Unit]
Description=TopoLight network monitoring
After=network-online.target
Wants=network-online.target

[Service]
User=topolight
Group=topolight
EnvironmentFile=/etc/topolight/topolight.env
ExecStart=/bin/sh -c 'exec /usr/local/bin/topolight $TOPOLIGHT_OPTS'
Restart=on-failure
RestartSec=3
LimitNOFILE=65536
AmbientCapabilities=CAP_NET_RAW CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_RAW CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/topolight
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable topolight >/dev/null 2>&1 || true
systemctl restart topolight
sleep 1
if systemctl is-active --quiet topolight; then
  IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
  say "TopoLight $("$BIN_DIR/topolight" -version | awk '{print $2}') is running."
  say "Open http://${IP:-<this-host>}:8432 and follow the setup wizard."
  say "Logs: journalctl -u topolight -f   ·   config: $CONF_DIR/topolight.env   ·   data: $DATA_DIR"
else
  die "service failed to start — journalctl -u topolight -n 50"
fi
