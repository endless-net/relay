# Tasks: relay

**Статус:** черновик локального планирования; реализация `not-verified`.

Владелец: `services/relay`. Общая фича: [specs/002-shared-go-checks](https://github.com/endless-net/workspace/blob/0586e302ac17214698f95d95cc04d3a63ae4a470/specs/002-shared-go-checks/spec.md).
Создание этих файлов не разрешает обход локальных правил, изменение чужого
кода, публикацию, запуск агентов или чатов. Перед кодом уточнить локальный
план, выполнить analyze/Guard и разрешить зависимости общей функции.

Источник T IDs: [root tasks](https://github.com/endless-net/workspace/blob/0586e302ac17214698f95d95cc04d3a63ae4a470/specs/002-shared-go-checks/tasks.md). Общие задачи workspace сюда не перенесены.

- [ ] T003 (C07, services/relay, FR-008/009) Подготовить reviewable предложение только своего Relay CI/profile; подтвердить public visibility и GitHub-hosted runner по STD-033, сохранить автономные product suites. Infrastructure CI/runners не изменять. Результат и ссылки сохранить в собственном specs/001-shared-go-checks/evidence.md; workspace собирает общую evidence.

- [ ] T018 [US2] (C07, services/relay, FR-003/004/008/009) После подтверждения visibility и runner-профиля/local specs подключить go.mod и relayapi/go.mod через scripts/check.sh и .github/workflows/ci.yml; сохранить собственные race/build/product suites, без production SPIRE и wire version изменений; exact evidence.

- [ ] T028 [US3] (C07, services/relay, FR-005/006/010) Пилотный owner добавляет свою реальную integration/contract suite в собственные tests/ и .github/workflows/ci.yml с общими helpers, required registration и cleanup; сохраняет success и все отрицательные extension outcomes в своих specs/001-shared-go-checks/evidence.md.

- [ ] T029 [US3] (C07, services/relay, FR-005/008/010) В owner specs/001-shared-go-checks/plan.md и .github/workflows/ci.yml назначить обязательные service/platform suites и полный aggregate; при отсутствии применимого extension объяснить N/A, заготовки pending. Доменные tests и wide System Tests не перемещать в Kit.

Перед выполнением уточнить local plan и проверки; завершение и evidence отмечает owner.
