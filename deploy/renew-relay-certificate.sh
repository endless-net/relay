#!/usr/bin/env bash
set -euo pipefail

certificate_name_file=/etc/endlessnet-relay/public-tls/cert-name
certificate_name=$(<"$certificate_name_file")
[[ "$certificate_name" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]]

exec /usr/bin/certbot renew \
  --cert-name "$certificate_name" \
  --standalone \
  --installer null \
  --preferred-challenges http \
  --pre-hook /usr/local/libexec/endlessnet-relay/prepare-certificate-renewal \
  --post-hook /usr/local/libexec/endlessnet-relay/restore-certificate-renewal \
  --deploy-hook /usr/local/libexec/endlessnet-relay/reload-certificate \
  --quiet
