#!/bin/sh
set -eu

case "${1:-}" in
  remove|deconfigure|0)
    if [ -d /run/systemd/system ]; then
      systemctl disable --now ghalet.service
    fi
    ;;
esac
