#!/bin/sh
set -eu

if ! getent group ghalet-runner >/dev/null; then
  groupadd --system ghalet-runner
fi
if ! id -u ghalet-runner >/dev/null 2>&1; then
  useradd --system --gid ghalet-runner --home-dir /var/lib/ghalet-runner \
    --shell /bin/bash ghalet-runner
fi
install -d -o ghalet-runner -g ghalet-runner -m 0700 /var/lib/ghalet-runner
install -d -o root -g root -m 0711 /var/lib/ghalet
install -d -o root -g root -m 0700 \
  /etc/ghalet /etc/ghalet/credentials /var/cache/ghalet /var/log/ghalet
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload
fi
