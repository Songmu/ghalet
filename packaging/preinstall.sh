#!/bin/sh
set -eu

case "${1:-}" in
  upgrade|[2-9]|[1-9][0-9]*)
    if [ -d /run/systemd/system ]; then
      systemctl stop ghalet.service
    fi
    ;;
esac
