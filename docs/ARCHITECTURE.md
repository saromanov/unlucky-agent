# unlucky-agent — архитектура

Версия документа: 0.1 (draft)
Язык реализации: Go.

---

## 1. Цели архитектуры

1. **Детерминизм.** Один и тот же вход даёт один и тот же вердикт. Всё недетерминированное
   вынесено в изолированные адаптеры и помечено в отчёте.
2. **Изоляция.** Разрушительные команды исполняются только внутри контейнера; ядро стенда
   физически не имеет пути выполнить их на хосте.
3. **Расширяемость без пересборки ядра.** Новый сценарий — YAML. Новый инструмент — отдельный
   процесс-адаптер.
4. **Потоковость.** Манифест 50k файлов пишется потоком, без загрузки дерева в память.
5. **Доказуемость.** Каждый вердикт восстановим из артефактов прогона.

## 2. Стек

- **Go 1.24+**, статическая сборка, CGO отключён по умолчанию (`CGO_ENABLED=0`).
- **CLI:** `spf13/cobra` (подкоманды, флаги, генерация справки).
- **Конфигурация сценариев:** `gopkg.in/yaml.v3` со строгим режимом (`KnownFields(true)`),
  чтобы опечатка в поле сценария была ошибкой, а не тихим дефолтом.
- **ФС-примитивы:** `io/fs`, `path/filepath`, `golang.org/x/sys/unix` (`Lstat`, `Stat_t`,
  `Listxattr`, `Flock`, `Ftruncate`).
- **Хеши:** `crypto/sha256` для идентичности, `golang.org/x/crypto/blake2b` для скорости
  хеширования крупных файлов. Выбор фиксируется в заголовке манифеста.
- **Docker:** на MVP — вызов CLI `docker` через `os/exec` с таймаутом и контекстом.
  Причина: меньше зависимостей, стабильный контракт, проще отладка. Интерфейс `sandbox.Driver`
  позволяет позже заменить на `github.com/docker/docker/client` без изменения ядра.
- **Шаблоны отчёта:** `text/template`. JSON — `encoding/json` со схемой.
- **Тесты:** стандартный `testing`, table-driven, golden-файлы, `go test -race`.
- **Сборка:** Makefile + GoReleaser для релизов.

Запрещённые зависимости: тяжёлые ORM, любые библиотеки, требующие cgo для базового сценария.

## 3. Схема компонентов

```
                        ┌──────────────────────────────┐
                        │            CLI               │
                        │  run | scenarios | reproduce │
                        │  adapter | doctor | report   │
                        └───────────────┬──────────────┘
                                        │
        ┌───────────────────────────────┼───────────────────────────────┐
        │                               │                               │
┌───────▼────────┐            ┌─────────▼──────────┐          ┌─────────▼─────────┐
│   scenario     │            │       runner       │          │      report       │
│ parse/validate │───────────▶│ plan → execute     │─────────▶│ json + markdown   │
└────────────────┘            └────┬──────────┬────┘          └───────────────────┘
                                   │          │
                    ┌──────────────▼──┐   ┌───▼───────────────┐
                    │    sandbox      │   │      oracle       │
                    │ docker driver   │   │ manifest/capture  │
                    │ guard, canary   │   │ diff/compare      │
                    │ limits, timeouts│   │ policy            │
                    └───────┬─────────┘   └───┬───────────────┘
                            │                 │
                    ┌───────▼─────────────────▼────────┐
                    │   container: workspace + home    │
                    │   /sandbox/{work,home,scratch,   │
                    │             evidence}            │
                    └───────┬──────────────────────────┘
                            │ exec / copy
                    ┌───────▼─────────┐
                    │    adapters     │
                    │ stdio JSON-RPC  │
                    │ tool-under-test │
                    └─────────────────┘
```

## 4. Раскладка репозитория

```
unlucky-agent/
  cmd/crashtest/main.go          # тонкий main, вызов internal/cli
  internal/
    cli/                         # cobra-команды
    scenario/                    # модель, парсер, валидатор, версии
    fixture/                     # генераторы фикстур, seed
    sandbox/                     # driver, guard, canary, limits, exec
    runner/                      # план прогона, исполнение, ретраи, шардирование
    adapter/                     # клиент протокола, реестр адаптеров
    oracle/                      # манифест, capture, diff, policy
    metrics/                     # расчёт метрик
    report/                      # json, markdown, schema
    shim/                        # cmdshim и barrier для сценариев
    version/                     # версия, отпечаток прогона
  adapters/                      # встроенные адаптеры (подкоманды)
    gitstash/
    cpr/
    tar/
    fssnapshot/
    perfect/                     # эталонная заглушка для проверки оракула
    gitcheckpoint/               # shadow-git инструменты (T1)
  scenarios/                     # S01..S22 в YAML
  schema/                        # JSON Schema отчёта и манифеста (go:embed)
  testdata/                      # golden-файлы, синтетические деревья
  docker/                        # Dockerfile.base, Dockerfile.tools
  docs/
  Makefile
  go.mod
  go.sum
```

## 5. Модель данных ядра

```go
type Scenario struct {
    ID         string
    Title      string
    Group      Group          // loss | meta | semantics | failure | cost | safety
    SpecVersion int
    Tiers      []Tier
    Platforms  []Platform
    Fixture    string
    Seed       int64
    Checkpoint CheckpointSpec
    Damage     []DamageStep
    Restore    RestoreSpec
    Expect     ExpectSpec
    Limits     Limits
    Resources  Resources
}

type DamageStep struct {
    Kind    StepKind // shell | toolcall | barrier | signal | fill_disk
    Shell   string
    Tool    *ToolCall
    Barrier string
    Timeout time.Duration
}

type ExpectSpec struct {
    Restore  []string
    Preserve []string
    Exclude  []string
    Require  MatchLevel // content | meta
}

type Verdict string // PASS | FAIL | SKIPPED_PLATFORM | SKIPPED_UNSUPPORTED | ERROR | FLAKY
```

Манифест не держится в памяти целиком:

```go
type Entry struct {
    Root    string            `json:"root"`
    Path    string            `json:"path"`
    Type    EntryType         `json:"type"`
    Mode    string            `json:"mode"`
    UID     uint32            `json:"uid"`
    GID     uint32            `json:"gid"`
    Size    int64             `json:"size"`
    Blocks  int64             `json:"blocks"`
    NLink   uint64            `json:"nlink"`
    Hash    string            `json:"hash,omitempty"`
    MtimeNS int64             `json:"mtime_ns"`
    Xattr   map[string]string `json:"xattr,omitempty"`
    Link    string            `json:"link,omitempty"`
}
```

Съёмка идёт в `bufio.Writer` поверх временного файла, записи сортируются по путям. Для
сортировки без загрузки в память при очень больших деревьях используется обход в порядке,
естественном для `filepath.WalkDir`, плюс пост-сортировка чанками: 50k путей удерживаются
в памяти (порядка 10 МБ), порог переключения на внешнюю сортировку — 1 млн записей.

## 6. Протокол адаптеров

Адаптер — отдельный исполняемый файл, найденный по имени в `adapters.yaml` или в `PATH`
как `crashtest-adapter-<id>`. Встроенные адаптеры доступны как `crashtest adapter <id>`.

Транспорт: построчный JSON-RPC 2.0 через stdin/stdout, UTF-8, один объект на строку.
stderr — логи, не парсится.

Методы:

- `handshake` → `{protocol: 1, id, tier, version, caps: ["checkpoint","restore","attribution","outside_project"]}`
- `prepare` `{ctx: SandboxCtx}` → `{}` — адаптер инициализирует своё хранилище внутри песочницы.
- `checkpoint` `{label}` → `{checkpoint_id, at_unix_nano}` — создать точку восстановления.
- `restore` `{checkpoint_id, mode}` → `{restored: bool, notes}` — выполнить откат.
- `attribution` `{since_checkpoint}` → `[{path, level, command}]`.
- `teardown` `{}` — освободить ресурсы, удалить служебные каталоги.

Правила:

- адаптер не имеет сетевого доступа;
- адаптер обязан быть идемпотентным при повторном `restore` на тот же `checkpoint_id`
  (иначе сценарий S15 фиксирует провал);
- адаптер не может быть интерактивным: запрещено читать stdin вне протокола;
- версия протокола несовместима → `ERROR` с явным сообщением, не тихий skip.

`SandboxCtx` содержит только пути внутри контейнера: `WorkDir`, `HomeDir`, `ScratchDir`,
`EvidenceDir`, `ToolchainDirs`.

Реестр:

```yaml
# adapters.yaml
adapters:
  - id: cline-checkpoints
    tier: git-shadow
    binary: cline
    version_pin: "3.4.1"
    image_digest: "sha256:..."
  - id: btrfs-snapshot
    tier: fs-snapshot
    binary: crashtest-adapter-btrfs
    requires: {fs: btrfs, root: true}
```

Версия адаптера попадает в отпечаток прогона. Несовпадение `version_pin` с фактической
версией — `ERROR`, а не предупреждение.

## 7. Песочница

### 7.1. Раскладка внутри контейнера

```
/sandbox/work       # проект, рабочая копия фикстуры
/sandbox/home       # эфемерный HOME (tmpfs), содержит ~/.config, ~/.ssh и т.п.
/sandbox/scratch    # крупные файлы, отдельный том с квотой
/sandbox/evidence   # единственный bind с хоста, rw, создан стендом
/opt/tools          # образы тестируемых инструментов, только чтение
```

Хостовые монтирования: только `/sandbox/evidence`. Всё остальное — tmpfs или named volume,
удаляемый после прогона.

### 7.2. Параметры запуска

```
docker run --rm
  --network none
  --user 1000:1000
  --cap-drop ALL
  --security-opt no-new-privileges
  --pids-limit 4096
  --memory <resources.memory_mb>
  --cpus 2
  --tmpfs /sandbox/home:rw,size=512m
  --mount type=bind,src=<out>/evidence,dst=/sandbox/evidence
  --mount type=bind,src=<fixture>,dst=/sandbox/work
```

`scratch` создаётся как отдельный volume с `size=` (tmpfs) либо как каталог в `work`,
если сценарий не требует квоты.

### 7.3. Guard

Перед запуском сценария `sandbox.Guard` выполняет внутри контейнера проверки:

1. корень ФС — контейнерный (`/.dockerenv` существует, либо в `/proc/self/mountinfo` нет
   хостового rootfs);
2. `$HOME` — внутри `/sandbox/home`;
3. в mountinfo нет хостовых путей, кроме `/sandbox/evidence`;
4. uid процесса — 1000;
5. `work` и `home` не являются симлинками на хостовые пути.

Любой провал — отказ запуска с кодом `GUARD_FAILED`. Guard исполняется до первой
разрушительной команды, а не после.

### 7.4. Canary

До прогона стенд создаёт на хосте набор маркеров (в `$HOME`, `/tmp`, `<out>/canary`) с
известным содержимым и хешами. После прогона проверяется неизменность. Провал — критическая
ошибка в отчёте и ненулевой код выхода, даже если все сценарии PASS.

### 7.5. Таймауты и kill

Каждый шаг порчи и откат исполняются с `context.WithTimeout`. По истечении — `docker kill`,
сбор логов, статус `ERROR` с причиной `timeout`. Процесс-адаптер получает SIGKILL, стенд
не ждёт корректного завершения.

## 8. Исполнение сценария

Последовательность для одной пары «сценарий × инструмент»:

1. **Plan.** Валидация сценария, проверка платформы и возможностей адаптера из `handshake`.
   Несовместимость → `SKIPPED_UNSUPPORTED` с причиной.
2. **Materialize.** Создание фикстуры по seed в пустом каталоге, вычисление ожидаемого
   манифеста `M0`.
3. **Start.** Запуск контейнера, guard, canary-базлайн.
4. **Prepare.** `adapter.prepare`.
5. **Checkpoint.** `adapter.checkpoint("T")`, снятие эталонного манифеста `MT`.
6. **Damage.** Выполнение шагов порчи по порядку. Каждый шаг пишется в журнал с таймингом.
7. **Damage manifest.** Снятие `MD` — подтверждение, что состояние действительно разрушено.
   Если `MD == MT`, сценарий дефектный → `ERROR(no_damage)`. Это защита от «зелёного»
   бенчмарка, который ничего не ломает.
8. **Restore.** `adapter.restore(cp, mode)` с таймаутом, замер времени.
9. **Final manifest.** Снятие `MF`.
10. **Compare.** Оракул строит diff `MT → MF` и проверяет `preserve`.
11. **Attribution.** Если сценарий требует — `adapter.attribution`.
12. **Teardown.** `adapter.teardown`, остановка контейнера, выгрузка evidence.
13. **Record.** Запись результата и метрик.

Между шагами 6 и 7 для конкурентных сценариев используются barrier-файлы.

## 9. Порча и детерминизм

### 9.1. Два режима

- **scripted** (по умолчанию) — шаги заданы в YAML. Детерминированно, воспроизводимо,
  используется в рейтинге.
- **agentic** — реальный агент с LLM выполняет свободную задачу. Помечается
  `non_reproducible`, в рейтинг не входит, нужен для демонстрации реалистичности.

Порча в scripted-режиме всё равно проходит через тот же интерфейс, что и у агента: либо
`shell` (оболочка внутри контейнера), либо `toolcall` (через `cmdshim`).

### 9.2. cmdshim

`cmdshim` — бинарник стенда, положенный первым в `PATH` внутри контейнера. Режимы:

- `cmdshim exec -- <cmd>` — исполняет команду как shell-вызов, пишет журнал;
- `cmdshim tool <name> --json <args>` — исполняет вызов «инструмента агента» и пишет журнал
  с признаком `via=tool`.

Различие `via=shell` против `via=tool` — основа сценария S10: инструменты, слушающие только
хуки инструмента, обязаны провалить мутации через shell.

### 9.3. Barrier

`shim/barrier` — примитив синхронизации на файлах и `unix.Flock`. Сценарии с двумя сессиями
и с torn state используют именованные барьеры: «сессия A дошла до точки X», «сессия B начала
писать». Без барьеров такие сценарии недетерминированы и уходят в FLAKY.

### 9.4. Управление временем и окружением

Фиксируется: `TZ=UTC`, `LANG=C.UTF-8`, `LC_ALL=C.UTF-8`, `SOURCE_DATE_EPOCH`, seed для
генератора фикстур. Время модификации файлов фикстуры задаётся явно (`unix.UtimesNanoAt`),
чтобы `meta_strict` был стабилен между прогонами.

## 10. Оракул

### 10.1. Съёмка

```
Walk(root):
  Lstat (без следования symlink)
  для regular: hash blake2b-256 потоком, буфер 1 МиБ
  blocks = st.Blocks (разреженность)
  xattr = Listxattr/Getxattr, ошибка EOPNOTSUPP → флаг unsupported
  mode = st.Mode, uid/gid из Stat_t
```

Параллелизм: worker pool по `GOMAXPROCS`, но запись строго последовательная и
отсортированная. Порядок обхода детерминирован: сортировка имён по байтам.

Хеширование 2 ГБ не должно съедать память: только потоковые буферы.

### 10.2. Diff

`oracle.Diff(base, observed, policy)` возвращает:

```go
type Diff struct {
    Missing   []Path   // было в base, нет в observed
    Added     []Path   // появилось (не всегда ошибка)
    Changed   []Path   // хеш или метаданные отличаются
    MetaOnly  []Path   // содержимое совпало, метаданные нет
    Clobbered []Path   // входит в preserve и изменилось
}
```

Память: diff по двум отсортированным потокам (merge-join), без построения двух карт на
50k+ файлов. Для 50k это не критично, но алгоритм сразу рассчитан на миллионы записей.

### 10.3. Policy

Политика — набор правил, полученных из сценария и платформы:

- `Exclude(glob)` — исключить из сравнения;
- `RequireMeta(bool)` — включать ли meta в гейт;
- `AllowAddedUnder(glob)` — какие новые пути допустимы (например, служебные каталоги);
- `Preserve(glob)` — что обязано выжить.

Политика — чистые данные, сериализуется в отчёт. Это позволяет пересчитать вердикт по
сохранённым манифестам без повторного прогона (`crashtest report --from <artifacts>`).

## 11. Метрики

`internal/metrics` получает `Diff` и тайминги, возвращает структуру метрик из ТЗ, раздел 8.
Формулы реализуются в одном месте, чтобы отчёт и JSON не расходились.

Агрегация по 5 прогонам: медиана для таймингов, консенсус для вердикта; расхождение
вердиктов → `FLAKY`, сценарий исключается из рейтинга и попадает в отдельный раздел.

## 12. Отчёт

- `report/json` — структура, сериализуемая в `encoding/json`, проверяется JSON Schema из
  `schema/report.v1.json` (schema встроена через `go:embed`, тест валидирует golden-отчёт).
- `report/markdown` — `text/template`, детерминированный вывод (сортировка всех списков).
- `crashtest reproduce <report.json>` — читает отпечаток, проверяет доступность образа и
  адаптеров, запускает тот же набор и сравнивает вердикты. Расхождение — отчёт «не
  воспроизвелось» с указанием, что именно разошлось.

Отпечаток прогона:

```go
type Fingerprint struct {
    CrashtestVersion string
    GoVersion        string
    SpecVersion      int
    FixtureHashes    map[string]string
    ImageDigest      string
    Adapters         map[string]string // id -> version
    Seed             int64
    Platform         string
}
```

## 13. Сборка и поставка

```
make build     # go build -trimpath -ldflags "-s -w -X ...version"
make test      # go test -race ./...
make lint      # gofmt -l, go vet, staticcheck (опционально)
make images    # docker buildx build base + tools
make e2e       # прогон MVP-матрицы в контейнере
make release   # goreleaser
```

Артефакты релиза: `crashtest` под linux/amd64, linux/arm64, darwin/arm64; SHA256SUMS;
SBOM; образ `ghcr.io/<org>/crashtest-base:<version>` с закреплённым digest.

Версионирование:

- `crashtest` — semver;
- спецификация сценариев — целое `spec_version`, повышается при любом изменении семантики
  сценария или оракула;
- схема отчёта и манифеста — отдельные целые, повышаются при несовместимом изменении полей.

Несовместимое изменение сценария обнуляет сравнимость результатов: старые отчёты помечаются
`outdated`, не смешиваются с новыми.

## 14. Тестовая стратегия

- **Unit.** Парсер сценариев (включая ошибки: неизвестное поле, неизвестный глоб), diff
  (синтетические деревья в `testdata`), policy, метрики, шаблоны отчёта (golden).
- **Integration.** Генераторы фикстур → манифест → diff на реальной ФС, без Docker.
  Проверяются метаданные: симлинки, права, разреженные файлы, xattr.
- **Sandbox.** Guard и canary на контейнере; тест «guard отказывает при bind-mount хоста»
  реализуется намеренно сломанной конфигурацией.
- **E2E.** MVP-матрица на `cp-r` и `perfect`: `perfect` обязан проходить всё, `none` —
  проваливать всё. Это проверка оракула.
- **Race и флаки.** `go test -race`, пять повторов E2E в CI, любой расходящийся вердикт —
  падение сборки.

## 15. Наблюдаемость

- Структурные логи в stderr ядра (`slog`), JSON, уровень `-v`.
- Каждый шаг прогона — событие с длительностью.
- Журнал порчи сохраняется в evidence (`damage.log`): команда, время, код возврата.
- Логи адаптера — отдельный файл на прогон, не смешивается с ядром.

## 16. Точки расширения

1. **Сценарий** — YAML, без кода.
2. **Адаптер** — внешний процесс, любой язык.
3. **Фикстура** — Go-генератор, регистрируется в `fixture` по имени.
4. **Дорожка** — добавляется значением `tier` и правилом сравнения в policy.
5. **Отчёт** — новый формат через интерфейс `report.Writer`.

## 17. Что явно не делаем в v1.0

- Веб-интерфейс и база данных: результаты — файлы в каталоге.
- Облачный сервис и аккаунты.
- Сравнение инструментов «в одном workspace одновременно»: прогоны изолированы.
- Автоматический лидерборд: публикация — вручную, после внешнего воспроизведения.
