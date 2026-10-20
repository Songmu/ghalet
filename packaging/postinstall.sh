#!/bin/sh
set -eu

if ! getent group ghalet >/dev/null; then
  groupadd --system ghalet
fi
if ! id -u ghalet >/dev/null 2>&1; then
  useradd --system --gid ghalet --home-dir /var/lib/ghalet --shell /bin/bash ghalet
fi
install -d -o root -g ghalet -m 0750 /etc/ghalet
install -d -o ghalet -g ghalet -m 0700 \
  /etc/ghalet/credentials /var/lib/ghalet /var/cache/ghalet /var/log/ghalet
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload
fi
