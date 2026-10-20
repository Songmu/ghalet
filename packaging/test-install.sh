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
    apt-get install -y --no-install-recommends "$package"
    ;;
  rpm)
    dnf install -y --setopt=install_weak_deps=False "$package"
    ;;
  *) echo "Unsupported package format: $format" >&2; exit 1 ;;
esac

test "$(id -u ghalet-runner)" -ne 0
test "$(stat -c %U:%G:%a /var/lib/ghalet-runner)" = ghalet-runner:ghalet-runner:700
test "$(stat -c %U:%G:%a /var/lib/ghalet)" = root:root:711
for path in /etc/ghalet /etc/ghalet/credentials /var/cache/ghalet /var/log/ghalet; do
  test "$(stat -c %U:%G:%a "$path")" = root:root:700
done
grep -qx 'User=root' /usr/lib/systemd/system/ghalet.service
grep -qx 'Group=root' /usr/lib/systemd/system/ghalet.service
grep -qx 'StandardOutput=append:/var/log/ghalet/ghalet.log' /usr/lib/systemd/system/ghalet.service
grep -qx 'StandardError=inherit' /usr/lib/systemd/system/ghalet.service
grep -qx 'KillMode=mixed' /usr/lib/systemd/system/ghalet.service
grep -qx 'TimeoutStopSec=15min' /usr/lib/systemd/system/ghalet.service
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
/usr/bin/ghalet config validate --config /etc/ghalet/config.yaml
grep -qx '  user: ghalet-runner' /etc/ghalet/config.yaml
touch /var/lib/ghalet/retained-state /etc/ghalet/credentials/retained-credential
chmod 0600 /etc/ghalet/credentials/retained-credential
# shellcheck disable=SC2016
su -s /bin/sh ghalet-runner -c '
  test "$HOME" = /var/lib/ghalet-runner &&
  test -w "$HOME" &&
  test -x /var/lib/ghalet &&
  test ! -w /var/lib/ghalet &&
  test ! -r /etc/ghalet/credentials/retained-credential &&
  test ! -w /etc/ghalet &&
  test ! -w /var/cache/ghalet &&
  test ! -w /var/log/ghalet
'
printf '%s\n' '{"msg":"package log rotation test"}' > /var/log/ghalet/ghalet.log
chmod 0600 /var/log/ghalet/ghalet.log
logrotate --force --state /var/lib/ghalet/logrotate.status /etc/logrotate.d/ghalet
test ! -s /var/log/ghalet/ghalet.log
grep -qx '{"msg":"package log rotation test"}' /var/log/ghalet/ghalet.log.1
test "$(stat -c %U:%G:%a /var/log/ghalet/ghalet.log.1)" = root:root:600
printf '%s\n' '# Retain local log rotation settings.' >> /etc/logrotate.d/ghalet
logrotate_sum=$(sha256sum < /etc/logrotate.d/ghalet)

case "$format" in
  deb) apt-get install -y --reinstall --no-install-recommends "$package" ;;
  rpm) dnf reinstall -y --setopt=install_weak_deps=False "$package" ;;
esac
test "$(sha256sum < /etc/ghalet/config.yaml)" = "$config_sum"
test "$(sha256sum < /etc/logrotate.d/ghalet)" = "$logrotate_sum"
test -e /var/lib/ghalet/retained-state
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
test -e /var/log/ghalet/ghalet.log.1
id ghalet-runner
