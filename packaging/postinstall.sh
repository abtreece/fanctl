#!/bin/sh
# Post-install: make systemd aware of the new unit. We intentionally do NOT
# enable/start fanctl automatically — the operator should review
# /etc/fanctl/config.yaml and run `fanctl probe` to confirm manual fan control
# is honored on their BMC first.
set -e

if [ ! -f /etc/fanctl/config.yaml ] && [ -f /etc/fanctl/config.example.yaml ]; then
    cp /etc/fanctl/config.example.yaml /etc/fanctl/config.yaml
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

cat <<'EOF'
fanctl installed. Next steps:
  1. Review /etc/fanctl/config.yaml
  2. fanctl doctor          # preflight checks
  3. fanctl probe           # confirm manual fan control is honored
  4. systemctl enable --now fanctl
EOF
