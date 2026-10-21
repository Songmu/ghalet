#!/bin/sh
set -eu

format=$1
package=$2

if [ -e /etc/ghalet ] || [ -e /var/lib/ghalet ] || [ -e /var/lib/ghalet-runner ] ||
  id -u ghalet >/dev/null 2>&1 || id -u ghalet-runner >/dev/null 2>&1; then
  echo "Package lifecycle tests require a disposable host without an existing ghalet installation." >&2
  exit 1
fi

case "$format" in
  deb)
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y --no-install-recommends "$package" < /dev/null
    ;;
  rpm)
    dnf install -y --setopt=install_weak_deps=False "$package" < /dev/null
    ;;
  *) echo "Unsupported package format: $format" >&2; exit 1 ;;
esac

test "$(id -u ghalet-runner)" -ne 0
test "$(stat -c %U:%G:%a /var/lib/ghalet-runner)" = ghalet-runner:ghalet-runner:700
test "$(stat -c %U:%G:%a /var/lib/ghalet)" = root:root:755
for path in /etc/ghalet /etc/ghalet/credentials /var/cache/ghalet /var/log/ghalet; do
  test "$(stat -c %U:%G:%a "$path")" = root:root:700
done
grep -qx 'Type=notify' /usr/lib/systemd/system/ghalet.service
grep -qx 'NotifyAccess=main' /usr/lib/systemd/system/ghalet.service
grep -qx 'User=root' /usr/lib/systemd/system/ghalet.service
grep -qx 'Group=root' /usr/lib/systemd/system/ghalet.service
grep -qx 'StateDirectory=ghalet' /usr/lib/systemd/system/ghalet.service
grep -qx 'StateDirectoryMode=0755' /usr/lib/systemd/system/ghalet.service
grep -qx 'CacheDirectory=ghalet' /usr/lib/systemd/system/ghalet.service
grep -qx 'CacheDirectoryMode=0700' /usr/lib/systemd/system/ghalet.service
grep -qx 'StandardOutput=journal' /usr/lib/systemd/system/ghalet.service
grep -qx 'StandardError=journal' /usr/lib/systemd/system/ghalet.service
grep -qx 'KillMode=mixed' /usr/lib/systemd/system/ghalet.service
grep -qx 'TimeoutStopSec=15min' /usr/lib/systemd/system/ghalet.service
grep -qx 'TimeoutStartSec=15min' /usr/lib/systemd/system/ghalet.service
grep -qx 'ExecReload=/usr/bin/ghalet apply --config /etc/ghalet/config.yaml' \
  /usr/lib/systemd/system/ghalet.service
grep -qx 'ExecStart=/usr/bin/ghalet run --config /etc/ghalet/config.yaml' \
  /usr/lib/systemd/system/ghalet.service
test ! -e /etc/systemd/system/multi-user.target.wants/ghalet.service
if command -v systemd-analyze >/dev/null 2>&1; then
  systemd-analyze verify /usr/lib/systemd/system/ghalet.service
fi
if [ -d /run/systemd/system ]; then
  test "$(systemctl show --property=ActiveState --value ghalet.service)" = inactive
fi
test ! -e /etc/ghalet/config.yaml
cp /usr/share/ghalet/config.yaml.example /etc/ghalet/config.yaml
config_sum=$(sha256sum < /etc/ghalet/config.yaml)
STATE_DIRECTORY=/var/lib/ghalet CACHE_DIRECTORY=/var/cache/ghalet \
  /usr/bin/ghalet config validate --config /etc/ghalet/config.yaml
grep -qx '  user: ghalet-runner' /etc/ghalet/config.yaml
grep -qx 'logs:' /etc/ghalet/config.yaml
grep -qx '  root: /var/log/ghalet/runners' /etc/ghalet/config.yaml
if grep -q '^defaults:' /etc/ghalet/config.yaml; then
  echo "Package configuration must not pin managed authentication." >&2
  exit 1
fi
touch /var/lib/ghalet/retained-state /etc/ghalet/credentials/retained-credential \
  /var/log/ghalet/retained-log
chmod 0600 /var/lib/ghalet/retained-state /etc/ghalet/credentials/retained-credential
# shellcheck disable=SC2016
su -s /bin/sh ghalet-runner -c '
  test "$HOME" = /var/lib/ghalet-runner &&
  test -w "$HOME" &&
  ls /var/lib/ghalet >/dev/null &&
  test -x /var/lib/ghalet &&
  test ! -w /var/lib/ghalet &&
  test ! -r /var/lib/ghalet/retained-state &&
  test ! -r /etc/ghalet/credentials/retained-credential &&
  test ! -w /etc/ghalet &&
  test ! -w /var/cache/ghalet &&
  test ! -w /var/log/ghalet
'

# Reinstallation must upgrade the legacy state parent without exposing files.
chmod 0711 /var/lib/ghalet
case "$format" in
  deb) apt-get install -y --reinstall --no-install-recommends "$package" < /dev/null ;;
  rpm) dnf reinstall -y --setopt=install_weak_deps=False "$package" < /dev/null ;;
esac
test "$(sha256sum < /etc/ghalet/config.yaml)" = "$config_sum"
test -e /var/lib/ghalet/retained-state
test -e /var/log/ghalet/retained-log
test "$(stat -c %U:%G:%a /var/lib/ghalet)" = root:root:755
test "$(stat -c %a /var/lib/ghalet/retained-state)" = 600
test "$(stat -c %a /etc/ghalet/credentials/retained-credential)" = 600
su -s /bin/sh ghalet-runner -c '
  ls /var/lib/ghalet >/dev/null &&
  test -x /var/lib/ghalet &&
  test ! -w /var/lib/ghalet &&
  test ! -r /var/lib/ghalet/retained-state &&
  test ! -r /etc/ghalet/credentials/retained-credential
'
if [ -d /run/systemd/system ]; then
  test "$(systemctl show --property=ActiveState --value ghalet.service)" = inactive
fi

case "$format" in
  deb) apt-get purge -y ghalet ;;
  rpm) dnf remove -y ghalet ;;
esac
test ! -e /usr/bin/ghalet
test ! -e /usr/lib/systemd/system/ghalet.service
test "$(sha256sum < /etc/ghalet/config.yaml)" = "$config_sum"
test -e /var/lib/ghalet/retained-state
test -e /etc/ghalet/credentials/retained-credential
test -e /var/log/ghalet/retained-log
id ghalet-runner
