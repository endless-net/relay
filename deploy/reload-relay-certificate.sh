#!/usr/bin/env bash
set -euo pipefail

certificate=/etc/endlessnet-relay/public-tls/fullchain.pem
private_key=/etc/endlessnet-relay/public-tls/privkey.pem
certificate_name=$(</etc/endlessnet-relay/public-tls/cert-name)
[[ "$certificate_name" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]]
openssl x509 -checkend 604800 -noout -in "$certificate" >/dev/null
openssl x509 -checkhost "$certificate_name" -noout -in "$certificate" >/dev/null
certificate_hash="$(openssl x509 -in "$certificate" -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum | awk '{print $1}')"
key_hash="$(openssl pkey -in "$private_key" -pubout -outform DER | sha256sum | awk '{print $1}')"
test "$certificate_hash" = "$key_hash"
systemctl try-restart endlessnet-relay.service
