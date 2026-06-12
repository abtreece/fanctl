#!/bin/sh
# Pre-remove: stop and disable the service, and make sure the BMC is back in
# automatic fan control so removing fanctl never leaves the fans pinned.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now fanctl.service || true
fi

# Best-effort restore of BMC automatic fan control.
if command -v ipmitool >/dev/null 2>&1 && [ -e /dev/ipmi0 ]; then
    ipmitool raw 0x30 0x30 0x01 0x01 || true
fi
