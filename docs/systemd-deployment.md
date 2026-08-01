# Развёртывание EndlessNet Relay под systemd

Эта инструкция описывает ручную установку одного Relay Coordinator и одного
или нескольких Relay-узлов из release-архива. Архив одинаков для обеих ролей и
не содержит production runtime-конфигурацию, реальную endpoint topology,
сертификаты, ключи или SPIRE registration entries.

## 1. Что находится в артефакте

Для каждой поддерживаемой архитектуры создаётся архив:

```text
endlessnet-relay_vX.Y.Z_linux_amd64.tar.gz
endlessnet-relay_vX.Y.Z_linux_arm64.tar.gz
checksums.txt
```

Внутри архива находятся:

- `endlessnet-relay` — публичный dataplane и relay mesh;
- `endlessnet-relay-coordinator` — control plane relay-кластера;
- `endlessnet-relay-smoke` — внешняя проверка TLS и Coordinator health;
- systemd units и helpers для обновления публичного сертификата;
- примеры `coordinator.env`, `relay.env`, `relay-instance.env` и
  `endpoints.json`;
- лицензии, build metadata и эта инструкция.

Бинарники статические (`CGO_ENABLED=0`), поэтому Go и системные shared libraries
на целевых серверах не нужны.

Собрать архивы локально:

```bash
bash scripts/build-release.sh v1.1.3 dist
sha256sum -c dist/checksums.txt
```

Для production следует использовать immutable artifact из утверждённого
GitHub Release: production-изменения этого проекта проходят через pull request
и guarded release/deploy workflows.

## 2. Зависимости и внешние контракты

### Общие для всех серверов

- Linux `amd64` или `arm64`, systemd и стандартные утилиты `tar`, `install`,
  `sha256sum`, `curl`;
- отдельный системный пользователь и группа `endlessnet-relay`;
- настроенный `wg-quick@wg0.service`: имеющиеся unit-файлы требуют именно
  интерфейс `wg0`;
- локальный `spire-agent.service`, сокет Workload API
  `/run/spire/sockets/agent.sock` и группа `spire-workload`;
- синхронизированное время и рабочий DNS.

Unit-файлы используют современные systemd sandboxing directives и
`LoadCredential`. Перед установкой их следует проверить на выбранном
дистрибутиве командой `systemd-analyze verify`. Существующий deployment
поддерживает Debian-family hosts.

SPIRE Server/Agent и reconciliation workload entries управляются внешним
infrastructure-контуром, а не этим репозиторием. До запуска сервиса он должен
выдать следующие точные identities:

| Процесс | Обязательная SPIFFE ID |
| --- | --- |
| Relay Coordinator | `spiffe://endlessnet.ru/service/relay-coordinator` |
| Relay с ID `<relay-id>` | `spiffe://endlessnet.ru/relay/<relay-id>` |
| Основной Coordinator | `spiffe://endlessnet.ru/service/coordinator` |

Identity Relay должна точно совпадать с `ENDLESSNET_RELAY_ID`. Общая identity
для нескольких unit-файлов недопустима. Все внутренние соединения используют
mTLS и TLS 1.3.

### Только для Relay Coordinator

- PostgreSQL. Текущий E2E baseline — PostgreSQL 17;
- выделенные database и login role; штатная схема использует локальный Unix
  socket и peer authentication;
- HTTPS-доступ к основному EndlessNet Coordinator, который реализует:
  `GET /internal/coordinator/relay-control/v1/trust-bundle` и
  `POST /internal/coordinator/relay-control/v1/authorize`;
- versioned endpoint snapshot JSON. Миграции PostgreSQL встроены в бинарник и
  применяются при каждом старте.

### Только для Relay-узла

- публичное DNS-имя и WebPKI certificate/key для него;
- приватная связность со всеми Relay peers по mesh;
- `certbot`, `openssl`, `bash` и `sha256sum` нужны только если используется
  поставляемый timer обновления Let's Encrypt certificate. Для сертификата,
  управляемого другим способом, timer устанавливать не нужно.

## 3. Сетевые потоки

| Назначение | По умолчанию | Доступ |
| --- | --- | --- |
| Relay public | TCP `9443` | Из клиентских сетей/Интернета |
| Relay mesh | TCP `9444` | Только между Relay через `wg0` |
| Relay Coordinator gRPC | TCP `9445` | Только от Relay через `wg0` |
| Relay Coordinator endpoints HTTPS | TCP `7078` | Обычно loopback, для основного Coordinator |
| Relay health/metrics | TCP `9190` | Loopback |
| Relay Coordinator health | TCP `9191` | Loopback |
| PostgreSQL | Unix socket | Локально на Coordinator host |

Сам WireGuard также требует разрешённый UDP-порт, выбранный инфраструктурной
конфигурацией. Он не задаётся приложением.

## 4. Общая подготовка хоста

Выберите архив под архитектуру сервера, скопируйте его и `checksums.txt`, затем
проверьте checksum до распаковки:

```bash
sha256sum -c checksums.txt --ignore-missing
```

Далее от `root`, подставив версию:

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

Убедитесь, что существуют `wg-quick@wg0.service`, `spire-agent.service`, группа
`spire-workload` и Workload API socket. Не запускайте приложение до выдачи
нужной SPIFFE identity.

## 5. Установка Relay Coordinator

### 5.1. PostgreSQL

Установите PostgreSQL и создайте выделенные role/database:

```bash
sudo -u postgres createuser --login --no-superuser --no-createdb \
  --no-createrole endlessnet-relay
sudo -u postgres createdb --owner=endlessnet-relay --encoding=UTF8 \
  --template=template0 endlessnet_relay
```

Если объекты уже существуют, не создавайте их повторно. В `pg_hba.conf` должен
быть разрешён `peer`-доступ локальной Unix-socket role `endlessnet-relay` к
database `endlessnet_relay`. Проверка выполняется от service user:

```bash
sudo -u endlessnet-relay psql \
  'postgresql:///endlessnet_relay?host=/var/run/postgresql&user=endlessnet-relay' \
  --command='SELECT 1'
```

### 5.2. Конфигурация и запуск

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

Отредактируйте:

- `ENDLESSNET_COORDINATOR_URL` — HTTPS base URL основного Coordinator;
- listen addresses, если Coordinator gRPC должен слушать только адрес `wg0`;
- `/etc/endlessnet-relay/endpoints.json`: `version` должен быть положительным,
  а `id` каждого endpoint должен совпадать с Relay ID. При изменении содержимого
  увеличивайте `version`; откат версии snapshot отвергается.

Endpoint snapshot читается только при старте, поэтому после его изменения нужен
restart Coordinator.

Запуск и проверка:

```bash
systemd-analyze verify /etc/systemd/system/endlessnet-relay-coordinator.service
systemctl daemon-reload
systemctl enable --now endlessnet-relay-coordinator.service
systemctl is-active --quiet endlessnet-relay-coordinator.service
curl --fail --silent http://127.0.0.1:9191/readyz
journalctl -u endlessnet-relay-coordinator.service -n 100 --no-pager
```

Сначала должен стать ready Relay Coordinator, и только после этого запускаются
Relay-узлы.

## 6. Установка каждого Relay-узла

### 6.1. Конфигурация и публичный сертификат

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

В `relay.env` укажите адрес Coordinator gRPC. В `relay-instance.env` укажите
уникальный Relay ID и адрес `wg0`, доступный остальным Relay. Оставьте
`ENDLESSNET_RELAY_BOOT_ID` пустым: процесс создаёт новый boot ID при каждом
старте. Замените `relay.example` реальным именем сертификата.

До запуска проверьте certificate, key и DNS:

```bash
openssl x509 -in /etc/endlessnet-relay/public-tls/fullchain.pem \
  -noout -checkhost relay.example
openssl x509 -in /etc/endlessnet-relay/public-tls/fullchain.pem \
  -noout -checkend 604800
```

### 6.2. Запуск и проверка

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

`readyz` становится успешным только после получения trust bundle от Relay
Coordinator. Запускайте Relay-узлы последовательно и проверяйте каждый перед
переходом к следующему.

### 6.3. Опциональное обновление Let's Encrypt certificate

Если сертификат получен Certbot и его имя совпадает с публичным DNS Relay:

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

Timer использует standalone HTTP-01 и при необходимости временно останавливает
`nginx.service`; TCP 80 должен быть доступен для ACME challenge.

## 7. Обновление и откат

Распакуйте новую версию в новый каталог `releases/vX.Y.Z`, не изменяя каталог
уже запущенной версии. Перед переключением сохраните текущий target:

```bash
CURRENT=$(readlink -f /opt/endlessnet-relay/current)
ln -sfn "$CURRENT" /opt/endlessnet-relay/.previous.next
mv -Tf /opt/endlessnet-relay/.previous.next /opt/endlessnet-relay/previous
ln -sfn /opt/endlessnet-relay/releases/vX.Y.Z \
  /opt/endlessnet-relay/.current.next
mv -Tf /opt/endlessnet-relay/.current.next /opt/endlessnet-relay/current
```

Перезапустите сначала Coordinator, проверьте `9191/readyz`, затем обновляйте
Relay-узлы по одному с проверкой `9190/readyz`.

Для отката атомарно верните `previous` и перезапустите соответствующий unit:

```bash
PREVIOUS=$(readlink -f /opt/endlessnet-relay/previous)
ln -sfn "$PREVIOUS" /opt/endlessnet-relay/.current.rollback
mv -Tf /opt/endlessnet-relay/.current.rollback /opt/endlessnet-relay/current
systemctl restart endlessnet-relay.service
```

На Coordinator host замените последнее имя unit на
`endlessnet-relay-coordinator.service`. Откат бинарника не откатывает endpoint
snapshot: snapshot version в PostgreSQL не может двигаться назад.
