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
if [ ! -e /etc/ghalet/config.yaml ]; then
  cat <<'EOF'
Configure ghalet before starting it:
  sudo install -o root -g root -m 0600 /usr/share/ghalet/config.yaml.example /etc/ghalet/config.yaml
  sudoedit /etc/ghalet/config.yaml
  sudo ghalet credential set default --prompt
  sudo ghalet config validate --config /etc/ghalet/config.yaml
  sudo systemctl enable --now ghalet

For a GitHub App, use ghalet credential set NAME --type github-app --client-id ID --file PATH.
Documentation: https://github.com/Songmu/ghalet/blob/main/docs/credentials.md
EOF
fi
