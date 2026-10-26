# Правила работы: relay

Репозиторий публичный и использует автономный локальный профиль по STD-030,
STD-035 и STD-036. Этот профиль заменяет прежний private include: чтение
Service Kit или приватного workspace, их credentials, код, workflows и образы
не являются обязательной зависимостью. Приватный профиль не копируется сюда.

- Перед изменениями прочитать [локальную конституцию](.specify/memory/constitution.md)
  и существующие документы затронутого контракта. Зафиксировать revision и dirty
  state прочитанных источников; недоступность необходимого источника назвать явно.
- Выполнять только запрошенную работу в этом репозитории. Сохранять чужие локальные
  изменения; соседний checkout или упоминание consumer не разрешает его менять.
  Commit/push, PR, выпуск, deployment и реальные инфраструктурные операции требуют
  соответствующего запроса. Skills без явного разрешения не изменять.
- Spec Kit запускать только по явному запросу пользователя; запрошенный этап
  не запускает следующие автоматически. Предложение и успешная сборка не означают
  принятие продукта или разрешение перейти к runtime.
- Схема, генерация и SDK принадлежат producer (STD-006/007). Consumer не создаёт
  ручную копию доменной модели. API/wire/runtime versions и migration status
  фиксировать отдельно; credentials не помещать в Git, логи или generated files.
- Для public CI использовать GitHub-hosted runners и публичные локальные configs
  (STD-015/029/033/035). Общие Bash/scripts/baseline checks выполняются в Ubuntu
  26.04; platform-specific проверки отдельно указывают OS и точный scope.
- При отказе необходимого инструмента или проверке с skip фиксировать not-verified,
  не pass. Для изменения кода выполнять применимые обязательные проверки всего
  затронутого модуля, а не только изменённых строк. Для документации проверить
  локальные ссылки и `git diff --check`.

## Проверки владельца

Существующий Go-модуль — `api/`; его локальный публичный lint config —
`api/.golangci-lint.yaml`, без загрузки private конфигурации. Из `api/` выполнять
последовательно:

```bash
goimports -w .
go vet ./...
golangci-lint run --config .golangci-lint.yaml ./... --timeout 1m
go test -short ./...
govulncheck ./...
go build ./...
```

Версии инструментов, Buf format/build/lint/breaking, repeat generation и
producer-specific fixtures описаны в [контрактном README](api/README.md).
Generated outputs не редактировать вручную; отсутствие published baseline
фиксировать явно. Новые модули не создавать ради формального запуска Go checks.

Внешний JSON dataplane отдельно проверяется командой из корня репозитория:

```bash
python3 api/checks/verify_public_wire.py
```

Зависимости JSON-проверки устанавливать вне репозитория в изолированную среду.
Контрактные shape/byte/semantic fixtures не подтверждают production signatures,
upstream authorization или TTL/revocation budgets. Relay передаёт opaque payload,
владеет registry/leases/fencing; Coordinator остаётся authority Network/Node pair,
Signing — purpose trust. Provision/host/deploy/activation находятся во внешнем
платформенном контуре вне активного состава; эти функции не переходят Relay или
Service Kit. Место и owner требуют отдельного решения COMPOSITION-Q-01.
