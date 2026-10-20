#!/bin/sh
set -eu

format=$1
package=$2

if [ -e /etc/ghalet ] || [ -e /var/lib/ghalet ] || id -u ghalet >/dev/null 2>&1; then
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

test "$(id -u ghalet)" -ne 0
test "$(stat -c %U:%G:%a /var/lib/ghalet)" = ghalet:ghalet:700
test "$(stat -c %U:%G:%a /var/cache/ghalet)" = ghalet:ghalet:700
test "$(stat -c %U:%G:%a /var/log/ghalet)" = ghalet:ghalet:700
grep -qx 'KillMode=mixed' /usr/lib/systemd/system/ghalet.service
grep -qx 'TimeoutStopSec=15min' /usr/lib/systemd/system/ghalet.service
grep -qx 'ExecStart=/usr/bin/ghalet run --config /etc/ghalet/config.yaml' \
  /usr/lib/systemd/system/ghalet.service
test ! -e /etc/systemd/system/multi-user.target.wants/ghalet.service
if [ -d /run/systemd/system ]; then
  systemd-analyze verify /usr/lib/systemd/system/ghalet.service
  test "$(systemctl show --property=ActiveState --value ghalet.service)" = inactive
fi
test ! -e /etc/ghalet/config.yaml
cp /usr/share/ghalet/config.yaml.example /etc/ghalet/config.yaml
config_sum=$(sha256sum < /etc/ghalet/config.yaml)
su -s /bin/sh ghalet -c '/usr/bin/ghalet config validate --config /etc/ghalet/config.yaml'
touch /var/lib/ghalet/retained-state /etc/ghalet/credentials/retained-credential

case "$format" in
  deb) apt-get install -y --reinstall --no-install-recommends "$package" ;;
  rpm) dnf reinstall -y --setopt=install_weak_deps=False "$package" ;;
esac
test "$(sha256sum < /etc/ghalet/config.yaml)" = "$config_sum"
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
id ghalet
