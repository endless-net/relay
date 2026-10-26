# Конституция relay

## Автономный публичный профиль

Публичный репозиторий поддерживает собственные локальные правила по STD-030,
STD-035 и STD-036. С версии 2.0.0 отменено обязательное чтение/подключение
приватной конституции и AGENTS Service Kit, включая remote fallback. Приватные
правила, реализация, CI/configs или images не копируются в публичный checkout.
Доступность private workspace и Service Kit не является условием работы здесь.
Это изменение подключения; API, runtime и права публикации не меняются.

Ответственность: opaque dataplane, registry/leases/epoch fencing; решения Network/Node pair принадлежат Coordinator, signing/trust purposes — Signing.

Описание текущего контракта — [локальный API README](../../api/README.md).

## Требования и проверки

- [Собственный AGENTS.md](../../AGENTS.md) задаёт workflow, применимые локальные
  команды и точные пределы проверок. Прямые инструкции пользователя имеют приоритет.
- STD-001/006/007: описывать наблюдаемое поведение и запрещённые исходы, сохранять
  producer ownership schemas/SDK, отдельные wire/API/runtime версии и явные
  migration differences. Неизвестные policy/timeout/limit values не выдумываются.
- STD-015/029/031/033/035: публичные автономные инструменты и configs,
  GitHub-hosted CI, Ubuntu 26.04 для общего baseline, отдельные target-OS checks.
  Неприменимая или невыполненная проверка не считается успешной.
- STD-016/018/020/023/024: authority определяется настоящим caller/resource/action,
  dependency failure не расширяет доступ; bounded deadline/cancellation/retry,
  immutable replay inputs, expiry и current-state checks у соответствующего owner.
  Shape/serialization fixtures не доказывают runtime authentication или durable replay.
- STD-034: локальная работа ограничена запрошенным owner scope; consumer references
  не разрешают менять чужой checkout. Host lifecycle/provision/deployment/
  activation/rollback относятся к внешнему платформенному контуру вне активного
  состава. Эти функции не переходят сервисам или Service Kit; место и owner
  требуют отдельного решения COMPOSITION-Q-01. Изолированный test fixture
  не получает production authority.
- Public consumer может получать private producer API по закреплённой версии с
  read-only credentials при сборке. Это не разрешает Service Kit dependency;
  credentials не попадают в Git/logs/artifacts и недоверенные external PR, а
  отсутствие необходимого доступа завершает fetch ошибкой без fallback.
- Применимость и exact revision источников и результатов проверок фиксируются;
  proposed решения не становятся accepted из-за документа или успешной сборки.

## Локальные дополнения

В этом разделе указывать только принятые особенности текущего репозитория:
причину, область, заменяемое общее положение и действующее локальное правило.
Отсутствие локального дополнения не отменяет применимое общее правило.
Подключение не принимает proposed решения, не разрешает запись в другие
репозитории, реализацию, commit/push или deployment вне запроса пользователя.

**Version**: 2.0.1 | **Ratified**: 2026-10-03 | **Last Amended**: 2026-10-26
