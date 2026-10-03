# Implementation Plan: relay

**Статус:** черновик локального планирования; реализация `not-verified`.

Владелец: `services/relay`. Общая фича: [specs/002-shared-go-checks](https://github.com/endless-net/workspace/blob/0586e302ac17214698f95d95cc04d3a63ae4a470/specs/002-shared-go-checks/spec.md).
Создание этих файлов не разрешает обход локальных правил, изменение чужого
кода, публикацию, запуск агентов или чатов. Перед кодом уточнить локальный
план, выполнить analyze/Guard и разрешить зависимости общей функции.

## Scope

- `C07` / `RB-relay`: Relay оба модуля и extensions. Контракт: N/A: dev/CI без изменения wire contracts. Owner autonomous product suites.

## Local context

Прочитать [AGENTS.md](../../AGENTS.md) и его includes/constitution.
Для Spec Kit явно выбрать `SPECIFY_FEATURE_DIRECTORY=specs/001-shared-go-checks`;
root ID и локальный ID могут различаться. Branch Sync не переключает копию;
выбор разрешённой `feature/002-shared-go-checks` выполняется в owner context.
Уточнить собственные файлы, интерфейсы, проверки и критерии до реализации.

## Standards and Verification

| STD | Applicability | Command / procedure | Expected result | Evidence / status |
| --- | --- | --- | --- | --- |
| STD-001 | root/owners | specify → plan → tasks → analyze, local artifacts до кода | FR/tasks/criteria связаны | root подготовлено; owners not-verified |
| STD-015 | base/extensions | success и failed/skipped/cancelled/missing/cleanup пилот | полный честный gate | owner runs not-verified |
| STD-029 | весь свой Go | version; config verify; run --config свежий Kit ./... по modules/variants | 2.14.0, fresh SHA, CI не правит source | owner lint logs not-verified |
| STD-030 | owner effective rules | полное чтение AGENTS/includes/constitutions | нет обхода локальных правил | конфликты в evidence |
| STD-031 | common scripts/CI | /etc/os-release и checks в 26.04 | actual Ubuntu 26.04 | root Spec Kit 26.04.1; owners not-verified |
| STD-032 | совместимость dev/CI | сверить принятый manifest/digest/версии | checks работают в общей среде | not-verified; пересборка вне scope |
| STD-033 | CI tests | runner guard и actual runs/group/labels/OS/isolation | класс соответствует подтверждённой visibility вызывающего repo, без смены класса | consumers и обновление guard not-verified |
| STD-034 | ownership/sync | python3 scripts/check-repository-ownership.py --plan specs/002-shared-go-checks/plan.md; Guard | точные targets и owner coverage | root validation отдельно; sync blocked |
| STD-007/021 | producer интерфейсы | diff review, supported adapter pairs | схемы/SDK не переносятся | runtime migration N/A; producer refinement pending |
| STD-023/026 | tokens/evidence/runners | read-only доступ, logs/artifact и untrusted-code policy tests | нет secrets/production authority | owners not-verified |

STD-002–014, 016–020, 022, 024–025, 027–028: runtime/storage/network/identity/
delivery поведение не меняется этим подключением. Применимые domain tests остаются
у owners; их исполнение не заявляется. STD-032 — проверка совместимости принятой
среды; эта задача не заявляет внедрение/пересборку devcontainers.

Каталог: [стандарты](https://github.com/endless-net/workspace/blob/0586e302ac17214698f95d95cc04d3a63ae4a470/docs/standards/README.md).
Команды workspace выполняются из корня workspace; локальные команды
и applicable проверки уточняет owner в этом плане до реализации.

## Dependencies

T001–T003 → T004 → fresh Guard/T005. US1 T006–T007 до активации consumers.
Producer T008–T013 → пилот Relay T018/T028; T014–T017 и T019–T025 подготавливаются
после принятия producer interface, выполняются независимо в своих owners.
T026–T027 и T018 → T028 → T029 → T030–T031. Заготовка без кода не закрывает SC-002/005.

- Решением пользователя от 3 октября снят общий main-only запрет в STD-030;
  актуальные профили Service Kit и Infrastructure также сняли прежние ограничения.
  G1 больше не блокирует feature Branch Sync. Перед sync остаются обязательными
  актуальный инвентарь, проверка targets, Guard и dry-run без переключения копий.
- Поправка STD-033 от 3 октября снимает безусловное противоречие с GitHub-hosted профилем Relay: он допустим для public. Перед CI записью подтвердить visibility каждого consumer; private требует self-hosted. Фактическое соответствие и общий runner guard остаются not-verified.
- У части owners scaffold constitution либо отсутствуют AGENTS/Spec Kit: определить
  effective workflow до кода; не устанавливать интеграции молча.
- Kit утверждает multi-module/checks-only/parity interface до consumers; это
  producer задача, не молчаливо принятый новый контракт.

- Прочитать effective AGENTS/includes/constitutions, оформить local Spec Kit и
  связать его с root feature. Разрешить локальные scope/Git/runner блокеры до записей.
- STD-001/015/029/030/031/033/034; STD-032 для совместимости среды;
  STD-023/026 для access/evidence; STD-007/021 при изменении producer interface.
  Проверки и N/A — из plan Standards verification; runtime STDs по своей применимости.
- Подключить producer-owned common contract после его проверки; immutable adapters/
  workflow refs и fresh main lint SHA независимы. Не копировать rules и не вызывать
  Kit check.sh на Kit source вместо consumer. Shared domain API не меняется.
- Local и CI baseline для всех own modules/variants; CI на push любой ветки, release
  только tag после exact commit checks. GitHub-hosted для public, self-hosted для private; без смены класса и skipped success;
  format CI без source mutation. Finite budgets/cleanup утверждает owner.
- Extensions не ослабляют baseline; весь required список/build legs входит в полный
  итог. Проверить failed/skipped/cancelled/missing/timeout/cleanup failures и fresh
  source outage; real suite требует real cleanup, не placeholder echo.
- Evidence exact owner commit + approved refs + resolved lint SHA + modules/variants
  + actual workflow/run/runner/group/OS/tools + base/extensions outcomes + gaps.
  Пройденные ordered local checks и CI обязательны отдельно; AI review не CI pass.
- При отсутствии собственного кода pending; не создавать empty module, runtime
  сервис или SDK копию ради подключения. Новые modules/язык требуют inventory review.

Owner permissions не возникают от наличия этой таблицы. Общий main-only запрет
снят решением пользователя; актуальный охват, producer readiness и проверка
runner-профиля остаются prerequisites sync/handoff; sources и результаты —
[evidence](https://github.com/endless-net/workspace/blob/0586e302ac17214698f95d95cc04d3a63ae4a470/specs/002-shared-go-checks/evidence.md).

Подробный общий дизайн: [plan.md](https://github.com/endless-net/workspace/blob/0586e302ac17214698f95d95cc04d3a63ae4a470/specs/002-shared-go-checks/plan.md). Все непроверенные gates сохраняют статус.

## Обсуждение до реализации

По прямому запросу пользователя публикуются только документы планирования.
Корневая постановка: [workspace PR #1](https://github.com/endless-net/workspace/pull/1).
Процедура обсуждения: [workspace PR #2](https://github.com/endless-net/workspace/pull/2).
Root feature revision: `a762e7220c7f45ee9913a95af7ca21550a89a753`;
source/правила публикации: `0586e302ac17214698f95d95cc04d3a63ae4a470`.
Spec/plan/tasks корневой задачи одинаковы в этих опубликованных revisions.
Роль: пилот после producer. Base владельца: `bbc3c4006f680fa131eb110481795d7e3de11e08`.
Видимость GitHub подтверждена как `PUBLIC`;
требуемый CI-класс: GitHub-hosted. Фактическая миграция CI не проверена.
Принятие глобального плана, producer ABI/refs, owner suites/budgets и локальная
готовность Spec Kit остаются implementation gates. Создание PR не закрывает задачи
и не разрешает изменение кода, CI, версий или production.
