# Deploying Relay with systemd

This guide describes manual installation of one Relay Coordinator and one or more
Relay nodes from a release archive. Both roles use the same archive. It contains
no production runtime configuration, actual endpoint topology, certificates, keys
or SPIRE registration entries.

## 1. Artifact contents

An archive is built for each supported architecture:

```text
endlessnet-relay_vX.Y.Z_linux_amd64.tar.gz
endlessnet-relay_vX.Y.Z_linux_arm64.tar.gz
checksums.txt
```

Each archive contains:

- `endlessnet-relay`: the public dataplane and relay mesh.
- `endlessnet-relay-coordinator`: the relay cluster control plane.
- `endlessnet-relay-smoke`: external TLS and Coordinator health checks.
- Systemd units and public certificate renewal helpers.
- Example `coordinator.env`, `relay.env`, `relay-instance.env` and `endpoints.json` files.
- Licenses, build metadata and this guide.

Binaries are static (`CGO_ENABLED=0`); target servers do not need Go or system shared libraries.

Build archives locally:

```bash
bash scripts/build-release.sh v1.1.3 dist
sha256sum -c dist/checksums.txt
```

For production, select an immutable artifact from an approved GitHub Release.
Changes pass through pull requests and the release workflow. Operators execute
rollouts; Relay does not initiate production deployment. The
[upstream contract](upstream-contract.md) describes compatible upstreams and
configurable SPIFFE identities.

## 2. Dependencies and external contracts

### Required on all servers

- Linux `amd64` or `arm64`, systemd, and standard `tar`, `install`, `sha256sum` and `curl` tools.
- A dedicated `endlessnet-relay` system user and group.
- A configured `wg-quick@wg0.service`; the supplied units require the `wg0` interface.
- A local `spire-agent.service`, Workload API socket at
  `/run/spire/sockets/agent.sock`, and the `spire-workload` group.
- Synchronized clocks and working DNS.

The supplied units use modern systemd sandboxing directives and `LoadCredential`.
Validate them on the chosen distribution with `systemd-analyze verify` before
installation. The supplied deployment setup targets Debian-family hosts.

The operator manages SPIRE Server/Agent and workload-entry reconciliation outside
this repository. Before starting services, provision exact identities matching
the configured policy. The defaults are:

| Process | Default SPIFFE ID |
| --- | --- |
| Relay Coordinator | `spiffe://endlessnet.ru/service/relay-coordinator` |
| Relay with ID `<relay-id>` | `spiffe://endlessnet.ru/relay/<relay-id>` |
| Compatible upstream | `spiffe://endlessnet.ru/service/coordinator` |

Operators can set a custom trust domain and service identities as described in the
[contract](upstream-contract.md). The Relay identity must match `ENDLESSNET_RELAY_ID`
exactly. Do not share an identity across different Relay units. All internal
connections use mTLS and TLS 1.3.

### Required only for Relay Coordinator

- PostgreSQL; the current E2E baseline uses PostgreSQL 17.
- A dedicated database and login role. The supplied setup uses a local Unix socket
  and peer authentication.
- HTTP/2 gRPC access with SPIFFE mTLS to an upstream implementing
  `endlessnet.relay.v1.RelayUpstreamService` as specified in the
  [protobuf upstream contract](upstream-contract.md).
- A versioned JSON endpoint snapshot. PostgreSQL migrations are embedded in the
  binary and applied at every startup.

### Required only for a Relay node

- A public DNS name and its WebPKI certificate/key.
- Private mesh connectivity to all Relay peers.
- `certbot`, `openssl`, `bash` and `sha256sum` only if using the supplied Let's
  Encrypt certificate renewal timer. Do not install the timer when certificates
  are managed another way.

## 3. Network flows

| Purpose | Default | Access |
| --- | --- | --- |
| Relay public | TCP `9443` | Client networks and the Internet |
| Relay mesh | TCP `9444` | Between Relay instances over `wg0` only |
| Relay Coordinator gRPC | TCP `9445` | From Relay instances over `wg0` only |
| Relay Coordinator endpoints HTTPS | TCP `7078` | Usually loopback, for the authorized upstream caller |
| Relay health/metrics | TCP `9190` | Loopback |
| Relay Coordinator health | TCP `9191` | Loopback |
| PostgreSQL | Unix socket | Local to the Coordinator host |

WireGuard also needs an allowed UDP port chosen by the operator's infrastructure
configuration. The application does not select that port.

## 4. Common host preparation

Choose the archive for the host architecture, copy it and `checksums.txt`, and
verify the checksum before extraction:

```bash
sha256sum -c checksums.txt --ignore-missing
```

As `root`, substitute the selected version:

```bash
VERSION=v1.1.3
ARCH=amd64
ARTIFACT="/var/tmp/endlessnet-relay_${VERSION}_linux_${ARCH}.tar.gz"
RELEASE_DIR="/opt/endlessnet-relay/releases/${VERSION}"

getent group endlessnet-relay >/dev/null ||
  groupadd --system endlessnet-relay
id -u endlessnet-relay >/dev/null 2>&1 ||
  useradd --system --gid endlessnet-relay --no-create-home \
    --shell /usr/sbin/nologin endlessnet-relay

install -d -o root -g root -m 0755 "$RELEASE_DIR"
tar -xzf "$ARTIFACT" -C "$RELEASE_DIR" --strip-components=1
install -d -o root -g endlessnet-relay -m 0750 /etc/endlessnet-relay
install -d -o endlessnet-relay -g endlessnet-relay -m 0750 \
  /var/lib/endlessnet-relay

ln -sfn "$RELEASE_DIR" /opt/endlessnet-relay/.current.next
mv -Tf /opt/endlessnet-relay/.current.next /opt/endlessnet-relay/current
```

Ensure `wg-quick@wg0.service`, `spire-agent.service`, the `spire-workload` group
and Workload API socket exist. Do not start the application before the required
SPIFFE identity is available.

## 5. Install Relay Coordinator

### 5.1. PostgreSQL

Install PostgreSQL and create a dedicated role and database:

```bash
sudo -u postgres createuser --login --no-superuser --no-createdb \
  --no-createrole endlessnet-relay
sudo -u postgres createdb --owner=endlessnet-relay --encoding=UTF8 \
  --template=template0 endlessnet_relay
```

Do not recreate objects that already exist. In `pg_hba.conf`, allow local
Unix-socket `peer` authentication for role `endlessnet-relay` to database
`endlessnet_relay`. Verify access as the service user:

```bash
sudo -u endlessnet-relay psql \
  'postgresql:///endlessnet_relay?host=/var/run/postgresql&user=endlessnet-relay' \
  --command='SELECT 1'
```

### 5.2. Configure and start

```bash
install -o root -g root -m 0600 \
  /opt/endlessnet-relay/current/examples/coordinator.env.example \
  /etc/endlessnet-relay/coordinator.env
install -o root -g endlessnet-relay -m 0640 \
  /opt/endlessnet-relay/current/examples/endpoints.example.json \
  /etc/endlessnet-relay/endpoints.json
install -o root -g root -m 0644 \
  /opt/endlessnet-relay/current/endlessnet-relay-coordinator.service \
  /etc/systemd/system/endlessnet-relay-coordinator.service
```

Edit:

- `ENDLESSNET_COORDINATOR_URL`: the upstream gRPC HTTPS origin.
- Listen addresses if Coordinator gRPC should bind only to the `wg0` address.
- `/etc/endlessnet-relay/endpoints.json`: `version` must be positive and each
  endpoint `id` must match its Relay ID. Content changes require an increased
  snapshot version; version rollback is rejected.

The endpoint snapshot is read only at startup, so changes require a Coordinator restart.

Start and verify:

```bash
systemd-analyze verify /etc/systemd/system/endlessnet-relay-coordinator.service
systemctl daemon-reload
systemctl enable --now endlessnet-relay-coordinator.service
systemctl is-active --quiet endlessnet-relay-coordinator.service
curl --fail --silent http://127.0.0.1:9191/readyz
journalctl -u endlessnet-relay-coordinator.service -n 100 --no-pager
```

Relay Coordinator must become ready before starting Relay nodes.

## 6. Install each Relay node

### 6.1. Configuration and public certificate

```bash
install -o root -g root -m 0600 \
  /opt/endlessnet-relay/current/examples/relay.env.example \
  /etc/endlessnet-relay/relay.env
install -o root -g root -m 0600 \
  /opt/endlessnet-relay/current/examples/relay-instance.env.example \
  /etc/endlessnet-relay/relay-instance.env
install -d -o root -g root -m 0700 /etc/endlessnet-relay/public-tls
ln -sfn /etc/letsencrypt/live/relay.example/fullchain.pem \
  /etc/endlessnet-relay/public-tls/fullchain.pem
ln -sfn /etc/letsencrypt/live/relay.example/privkey.pem \
  /etc/endlessnet-relay/public-tls/privkey.pem
install -o root -g root -m 0644 \
  /opt/endlessnet-relay/current/endlessnet-relay.service \
  /etc/systemd/system/endlessnet-relay.service
```

Set the Coordinator gRPC address in `relay.env`. In `relay-instance.env`, set a
unique Relay ID and a `wg0` address reachable by other Relay instances. Leave
`ENDLESSNET_RELAY_BOOT_ID` empty so the process generates a new boot ID at every
startup. Replace `relay.example` with the actual certificate name.

Check the certificate, key and DNS before startup:

```bash
openssl x509 -in /etc/endlessnet-relay/public-tls/fullchain.pem \
  -noout -checkhost relay.example
openssl x509 -in /etc/endlessnet-relay/public-tls/fullchain.pem \
  -noout -checkend 604800
```

### 6.2. Start and verify

```bash
systemd-analyze verify /etc/systemd/system/endlessnet-relay.service
systemctl daemon-reload
systemctl enable --now endlessnet-relay.service
systemctl is-active --quiet endlessnet-relay.service
curl --fail --silent http://127.0.0.1:9190/healthz
curl --fail --silent http://127.0.0.1:9190/readyz
curl --fail --silent http://127.0.0.1:9190/metrics
journalctl -u endlessnet-relay.service -n 100 --no-pager
```

`readyz` requires a running listener, valid instance lease, usable trust from
Relay Coordinator and no fencing or draining. Start Relay nodes sequentially,
checking each before proceeding to the next.

### 6.3. Optional Let's Encrypt certificate renewal

If Certbot issued the certificate and its name matches the Relay public DNS name:

```bash
install -d -o root -g root -m 0755 /usr/local/libexec/endlessnet-relay
install -o root -g root -m 0755 \
  /opt/endlessnet-relay/current/renew-relay-certificate.sh \
  /usr/local/libexec/endlessnet-relay/renew-certificate
install -o root -g root -m 0755 \
  /opt/endlessnet-relay/current/reload-relay-certificate.sh \
  /usr/local/libexec/endlessnet-relay/reload-certificate
install -o root -g root -m 0755 \
  /opt/endlessnet-relay/current/prepare-relay-certificate-renewal.sh \
  /usr/local/libexec/endlessnet-relay/prepare-certificate-renewal
install -o root -g root -m 0755 \
  /opt/endlessnet-relay/current/restore-relay-certificate-renewal.sh \
  /usr/local/libexec/endlessnet-relay/restore-certificate-renewal
printf '%s\n' relay.example >/etc/endlessnet-relay/public-tls/cert-name
chmod 0600 /etc/endlessnet-relay/public-tls/cert-name
install -o root -g root -m 0644 \
  /opt/endlessnet-relay/current/endlessnet-relay-cert-renew.service \
  /opt/endlessnet-relay/current/endlessnet-relay-cert-renew.timer \
  /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now endlessnet-relay-cert-renew.timer
systemctl list-timers endlessnet-relay-cert-renew.timer
```

The timer uses standalone HTTP-01 and temporarily stops `nginx.service` when
necessary. TCP port 80 must be reachable for the ACME challenge.

## 7. Upgrade and rollback

Extract a new version into a new `releases/vX.Y.Z` directory without modifying
the running version's directory. Preserve the current target before switching:

```bash
CURRENT=$(readlink -f /opt/endlessnet-relay/current)
ln -sfn "$CURRENT" /opt/endlessnet-relay/.previous.next
mv -Tf /opt/endlessnet-relay/.previous.next /opt/endlessnet-relay/previous
ln -sfn /opt/endlessnet-relay/releases/vX.Y.Z \
  /opt/endlessnet-relay/.current.next
mv -Tf /opt/endlessnet-relay/.current.next /opt/endlessnet-relay/current
```

Restart Coordinator first and check `9191/readyz`. Then update Relay nodes one at
a time, checking `9190/readyz` after each update.

To roll back, atomically restore `previous` and restart the corresponding unit:

```bash
PREVIOUS=$(readlink -f /opt/endlessnet-relay/previous)
ln -sfn "$PREVIOUS" /opt/endlessnet-relay/.current.rollback
mv -Tf /opt/endlessnet-relay/.current.rollback /opt/endlessnet-relay/current
systemctl restart endlessnet-relay.service
```

On a Coordinator host, use `endlessnet-relay-coordinator.service` in the last
command. Binary rollback does not roll back the endpoint snapshot: its version
in PostgreSQL cannot move backwards.

The `endlessnet-relay-smoke` utility accepts `--trust-domain` and
`--coordinator-identity` with the same exact identity policy as the runtime.
Pass a custom domain explicitly; the default identity is derived from that domain.
Contradictory settings are rejected before network checks.
