#!/usr/bin/env bash
set -euo pipefail

state_dir=/run/endlessnet-relay-cert-renew
install -d -m 0700 -o root -g root "$state_dir"
rm -f -- "$state_dir/nginx-was-active"
if systemctl is-active --quiet nginx.service; then
  : >"$state_dir/nginx-was-active"
  chmod 0600 "$state_dir/nginx-was-active"
  systemctl stop nginx.service
fi
