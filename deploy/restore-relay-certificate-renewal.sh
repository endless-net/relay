#!/usr/bin/env bash
set -euo pipefail

state=/run/endlessnet-relay-cert-renew/nginx-was-active
if [ -f "$state" ] && [ ! -L "$state" ]; then
  rm -f -- "$state"
  systemctl start nginx.service
fi
