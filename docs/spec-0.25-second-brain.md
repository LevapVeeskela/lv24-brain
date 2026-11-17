# Спецификация: lv24-brain 0.25 — Second Brain Correctness

Status: draft for review
Date: 2026-11-17
Related: `docs/audit-2026-11-17.md`, `docs/release-0.24.0.md`, `docs/localapi-v1.md`
Scope: 0.24.1 (hotfix) + 0.25.0 (correctness) + 0.25.1 (perf follow-up)

Допущения: один владелец-человек, три машины (DESKTOP-F5INSH6, BY-BST-DEV15,
macbook-pro-veeskela), несколько агентских сессий на машину, 46 проектов,
git остаётся canonical. Server-canonical и семантический поиск — вне 0.25
(см. Non-goals).

---

## 1. Проблема

Аудит 0.24.0 показал: каркас (provenance, review, locks, localapi, MCP policy)
крепкий, но продукт не является «вторым мозгом» по трём причинам:

1. **Retrieval не находит нужное.** Только BM25; веса recency/pin не работают;
   бюджет context молча выбрасывает записи; `as-of`/`since` несогласованы.
2. **Ingestion мёртв, импорт неполон.** 8/8 коннекторов падают на секретах;
   engram-импорт теряет prompts, per-note timestamps, path_provenance.
3. **Изоляция и конкурентность не гарантированы по умолчанию.** Journal/prompt
   видят все проекты; alias/path коллизии; consolidate/sync гоняются с писателями.

Цель 0.25 — сделать так, чтобы на вопрос «что я знаю по этой задаче в этом
проекте» система отвечала **полно, воспроизводимо и без утечек между проектами**,
а три машины и несколько сессий писали без потерь.

## 2. Goals / Non-goals

### Goals
- G1: контекст по задаче содержит релевантные решения/попытки, а не брендинг,
  и явно сообщает, что выброшено.
- G2: мульти-проектные поверхности по умолчанию изолированы; коллизии
  alias/ID/path невозможны.
- G3: конкурентные записи (3 машины × N сессий) не теряют данные; конфликты
  видны и разрешимы.
- G4: импорт/экспорт идемпотентен и не теряет prompts, timestamps, provenance.
- G5: хуки всех трёх хостов работают и диагностируются.
- G6: desktop закрывает 7 недостающих поверхностей и не врёт в статусах.
- G7: latency context < 1 c на 1.7k записей/проект; pull-reindex < 4 c.

### Non-goals (0.25)
- Эмбеддинги/векторный поиск и reranker — 0.26 (после eval-базлайна).
- Entity layer (люди/организации) и reflection/суммаризация — 0.26.
- Server-canonical, multi-user, multi-replica — отдельное решение.
- Мобильный/веб-клиент, Linux-пакеты desktop.
- Автоматическое разрешение конфликтов без человека.

## 3. Роли и сценарии

- **S1. Агент в проекте A** просит контекст по задаче; не должен видеть записи B.
- **S2. Две сессии на одной машине** пишут заметки и state одновременно.
- **S3. Три машины** делают sync во время записи; ни одна запись не теряется.
- **S4. Владелец** импортирует engram, затем курирует; повторный импорт не
  воскрешает trash и не меняет `updated_at`.
- **S5. Владелец** открывает desktop после конфликта sync и разрешает его.
- **S6. Новый проект** регистрируется, пути получают provenance, alias не
  конфликтует.
- **S7. Агент завершает сессию** — handoff и decisions появляются на всех хостах.

## 4. Workstreams и требования

### WS1. Retrieval и контекст

| ID | Требование | Приёмка |
|---|---|---|
| RET-1 | BM25 нормализуется на запрос: `norm = bm25 / (bm25 + median_bm25_matched)`, clamp [0,1] | Синтетический корпус: свежая запись с BM25 в 1.2× хуже старой попадает выше; тест на доли весов |
| RET-2 | Веса `0.55/0.20/0.15/0.05/0.05` применяются к нормализованным сигналам; вклад каждого виден в `--explain` | `--explain` печатает share каждого сигнала; сумма = 1 |
| RET-3 | Если strict AND дал < K (K=5), выполняется OR-запрос; результаты объединяются и дедуплицируются | Запрос «retrieval ranking» находит ≥3 записи вместо 1; тест OR-фолбэка |
| RET-4 | Query-layer пакуется первым; капы слоёв пропорциональны бюджету и могут заимствовать друг у друга | `--budget 4000` включает ≥1 решение/попытку по запросу; `--budget 32000` масштабируется |
| RET-5 | Нет фиксированного `contextMaxRecords=8`; ограничение — бюджет | `--budget 6000` возвращает >8 записей при наличии |
| RET-6 | Markdown-сводка печатает `… N more dropped` по каждому слою; JSON `truncated[]` сохраняется | Тест: budget 200/500 не превышает лимит, dropped виден в тексте |
| RET-7 | Токены считаются как `max(4 runes/token, эвристика по алфавиту)`: кириллица ≈ 2 символа/токен | Тест на русском тексте: summary ≤ budget с допуском 5% |
| RET-8 | Trust влияет на ранжирование: `verified > stated > inferred`; inferred-only контекст помечается | Тест: verified-решение выше inferred при равном BM25 |
| RET-9 | MCP `truncated` ≤ 20 записей + `total_truncated`; structuredContent ≤ 3× бюджета | MCP context на budget 3000 ≤ ~12 KB |
| RET-10 | MCP search/context получают `as_of`, `include_superseded`, `subtype`, `scope`, `topic` | Схема инструментов и тест паритета с CLI |

### WS2. Temporal correctness

| ID | Требование | Приёмка |
|---|---|---|
| TMP-1 | `--since` во всех поверхностях означает `updated_at` (search, context, list) | Один и тот же набор при одинаковом окне |
| TMP-2 | `--as-of` применяется к records/handoff; state/findings помечаются `state_as_of: "current"` в JSON и тексте | `--as-of 2026-11-08` не выдаёт goal от 11-17 без пометки |
| TMP-3 | `--as-of` не влияет на state/findings молча: вывод содержит предупреждение | Тест на текст и JSON |
| TMP-4 | Per-record `updated_at` читается из маркера записи, а не из файла | Заметки дневного файла имеют разные timestamps |

### WS3. Мульти-проектная изоляция

| ID | Требование | Приёмка |
|---|---|---|
| PRJ-1 | `SaveProject` отклоняет: alias == чужой ID, alias == чужой alias, path == чужой path | Тесты на каждую коллизию; ошибка с именами конфликтующих проектов |
| PRJ-2 | `search --project` резолвит ID/alias/path через `ResolveSelector`; неизвестный — ошибка exit 2, не 0 hits | `lv24-brian`, `LV24-BRAIN`, alias — ошибка/успех по правилам |
| PRJ-3 | `brain_journal` и `brain_prompt search` без проекта — ошибка; нужен `--project` или `--all` | MCP-схема и CLI совпадают с поведением |
| PRJ-4 | `discover --apply` обновляет `repoIndex`/`knownPaths` в цикле; второй клон того же repo не регистрируется | Тест: два клона за один скан → 1 проект + предупреждение |
| PRJ-5 | `projectsByRepo` при коллизии выбирает детерминированно (explicit mapping) и пишет warning; дамп не приписывается случайному проекту | Тест на lv24-ytk |
| PRJ-6 | Multi-project GitHub/GitLab connection: per-repo mapping `owner/repo:project` или отказ при `len(Projects)>1` | Items получают `project_id`; grant совпадает |
| PRJ-7 | `brain project rm <id>`: tombstone, перенос/trash записей, rebuild индекса | Команда идемпотентна; записи не теряются |
| PRJ-8 | Doctor проверяет зарегистрированные пути: owned/missing по машинам | `brain doctor` показывает missing-пути; adopt не считает их owned |
| PRJ-9 | Несуществующий путь под проектом — ошибка, не резолв в объемлющий проект | `brain open .\nope` → exit 1 |
| PRJ-10 | Дефолт MCP/desktop в мульти-проектном brain — require explicit scope (флаг или allowlist) | Без scope — ошибка с подсказкой |

### WS4. Мульти-сессионная конкурентность

| ID | Требование | Приёмка |
|---|---|---|
| CON-1 | `merge apply` берёт per-file lock на каждый целевой файл и перепроверяет fingerprint плана; при расхождении — `stale_plan`, без записи | Тест: писатель между plan и apply → 0 потерь |
| CON-2 | Неудачный `git stash pop` не дропает stash: ref сохраняется, sync завершается ошибкой с инструкцией | Тест с конфликтом в worktree |
| CON-3 | Единый `sync.lock` (owner-aware: pid+host+TTL); publish и sync используют один механизм | Тест: publish не крадёт lock у sync |
| CON-4 | Писатели берут shared sync-guard; sync/consolidate — exclusive; таймаут с понятной ошибкой | Тест writer-during-sync: запись либо до, либо после, не внутри |
| CON-5 | MCP state использует `SetStateIfRevision`; CLI `state set --expected-revision` | Два процесса: второй получает 409 |
| CON-6 | Journal: cross-process lock (или append с O_APPEND+fsync), чтение дедуплицирует по `(ts,event,id)` | Два процесса пишут 100 событий — 0 дублей, 0 потерь |
| CON-7 | Merge state сохраняет `fresh_until` и provenance (max updated_at), при расхождении — conflict record | Тест на frontmatter |
| CON-8 | Registry: расхождение скаляров (name/kind/repo_url/branch) → conflict record, не тихая потеря | Тест |
| CON-9 | Удалённые paths/aliases не воскрешаются при merge (tombstone/removal-wins) | Тест |
| CON-10 | Doctor/review игнорируют `merges/*.json` со `status=applied` | 21 ложный конфликт исчезает из вывода |

### WS5. Импорт и exchange

| ID | Требование | Приёмка |
|---|---|---|
| IMP-1 | engram import: plan → commit; при любом конфликте `--apply` ничего не пишет (или `--skip-conflicts`) | Прогон на копии: 0 файлов при конфликте; повтор идемпотентен |
| IMP-2 | Per-note `updated_at` сохраняется (`source.updatedAt` в маркере) | 2902 заметки получают разные timestamps; `--since` работает |
| IMP-3 | Prompts: opt-in экспорт в зашифрованный локальный бандл; документация о потере | 1189 prompts доступны для backup; git их не содержит |
| IMP-4 | Exchange v2: `projects.jsonl` содержит `path_provenance`, `pinned`; импорт v1 совместим | Round-trip export→import→export идентичен по hash |
| IMP-5 | NULL-project строки маппятся по директории сессии; если не резолвится — quarantine с причиной | 101 строка: 0 в engram-unmapped при известной директории |
| IMP-6 | `brain record move <id> --to <project>` переносит запись и индекс | Команда идемпотентна |
| IMP-7 | Quarantine inbox: `brain review --quarantine` + bulk review | 33 trashed/70 quarantined доступны для ревью |
| IMP-8 | Import dry-run не пишет ничего (включая backups/) и не хеширует `index/` | mtime index/ неизменен |
| IMP-9 | Manifest SHA256 проверяется при импорте; `--rollback` реализован или удалён | Тест на подмену |

### WS6. Хуки и agent loop

| ID | Требование | Приёмка |
|---|---|---|
| HOK-1 | Claude Code: `createIfMissing:true`; `--check` различает created/installed/not-installed | `brain agents sync --hooks` создаёт settings.json |
| HOK-2 | Hook log: одна JSON-строка на вызов в `index/hooks.log` (опционально `BRAIN_HOOK_LOG`); stderr не теряется | Видно последний запуск и ошибку |
| HOK-3 | `brain hooks status --agents`: host → installed/trusted/last-run/last-error | Doctor и desktop показывают статус |
| HOK-4 | OpenCode передаёт stdin JSON → `Decision:` собираются | Тест: 2 decisions из финального сообщения |
| HOK-5 | Codex trust drift: сравнение hash, предупреждение и команда re-approve | Изменённый exe → warning |
| HOK-6 | Хуки не пишут при активном sync (shared guard) или пишут атомарно | Нет handoff без journal-строки |
| HOK-7 | Бэкапы: только файлы без brain-маркера, не перезаписывать, chmod 0600 | Секреты не копируются в лишний файл |
| HOK-8 | JSON rewrite сохраняет EOL и duplicate keys | CRLF settings.json не даёт whole-file diff |

### WS7. Desktop

| ID | Требование | Приёмка |
|---|---|---|
| DSK-1 | `/quarantine` имеет корректный title/app bar | Тест route title |
| DSK-2 | Git sync: plan → apply → rollback, per-path действия, pull-only | Конфликт разрешается без CLI |
| DSK-3 | Переключение проекта в rail синхронизирует URL, store и state | Тест навигации |
| DSK-4 | State 409: диалог «перезагрузить/перезаписать», черновик не теряется | Тест |
| DSK-5 | `fresh_until` отображается и редактируется; stale-бейдж | UI-тест |
| DSK-6 | Projects: все пути (owned/unknown/missing), adopt/onboard из UI | Кнопка adopt вызывает API |
| DSK-7 | Digest: endpoint + кнопка в Inbox | Proposals генерируются из UI |
| DSK-8 | Error boundary; refresh по focus/visibility; last-updated | Тест |
| DSK-9 | Переводы zh/ru для snooze/filter; a11y: h1, focus trap, aria-activedescendant, lang | Проверка ключей и клавиатуры |
| DSK-10 | Удалить мёртвый CSS (shell-body/app-nav/brand/wordmark) | Нет неиспользуемых селекторов |

### WS8. Производительность

| ID | Требование | Приёмка |
|---|---|---|
| PRF-1 | Linked candidates строятся из уже загруженного каталога, без `GetRecord` на запись | Context lv24 3.6 c → ≤ 1 c |
| PRF-2 | `wal_checkpoint(TRUNCATE)` + FTS `optimize` после Reindex | WAL ≤ 5 MB после sync |
| PRF-3 | Reindex одной транзакцией + prepared statement | rebuild ≤ 3 c на текущем корпусе |
| PRF-4 | Инкрементальный reindex в sync по `git diff --name-only` | pull-reindex ≤ 1 c |
| PRF-5 | Мемоизация листингов в рамках команды; убрать дублирующие сканы findings | review −30% |
| PRF-6 | Keyset-пагинация поиска вместо перечитывания top-N | limit 200 ≤ 40 ms |

### WS9. Ops, docs, release

| ID | Требование | Приёмка |
|---|---|---|
| OPS-1 | PyPI в `release.yml` или post-release verify; desktop — tag-trigger | Публикация без ручного шага |
| OPS-2 | `docs/install` и runbook соответствуют версиям; generated MCP tool list | Проверка версии в CI |
| OPS-3 | `desktop.yml` default engine = текущий; guard `engine >= pin` | Ошибка при старом default |
| OPS-4 | CHANGELOG + release notes из `docs/release-X.md` | Release body содержит заметки |
| OPS-5 | `brain backup` / `brain verify` + DR-runbook (local/git) | Восстановление на чистой машине |
| OPS-6 | Docs CI (build+lint+check) и semantic checks в `check.mjs` | Битая команда ломает CI |
| OPS-7 | Desktop rollback задокументирован | Страница + проверка |

## 5. Изменения данных и совместимость

- **Схема записей:** без breaking changes. Добавляется опциональный маркер
  `updated_at` на запись; старые файлы читаются как раньше.
- **Registry:** без новых полей; добавляются инварианты уникальности.
- **Exchange:** `schema_version: 2`; v1 импортируется; v2 экспортирует
  `path_provenance`, `pinned`, per-note timestamps.
- **Journal:** без смены формата; добавляется lock и дедуп при чтении.
- **Local API:** только аддитивные операции; golden operation set обновляется
  осознанно (`UPDATE_LOCALAPI_CONTRACT=1`).
- **Миграции:** `brain migrate engram --repair-timestamps` (идемпотентно),
  `brain index --rebuild` после PRJ-5/PRJ-7.

## 6. Контракты

### CLI (новое/изменённое)
```
brain state set --expected-revision <rev>
brain journal [--project <id> | --all]
brain prompt search [--project <id> | --all]
brain project rm <id> [--move-records-to <id>]
brain record move <id> --to <project>
brain hooks status [--agents] [--json]
brain connection test <id> [--json]
brain connection show <id>
brain search --project <id|alias|path> [--all]
brain review --quarantine [--bulk]
brain migrate engram --repair-timestamps
```

### Local API (аддитивно)
```
POST /v1/projects/adopt            {apply: bool} -> AdoptReport
POST /v1/projects/{id}/paths       {path} -> Project
POST /v1/digest                    {project?, write?} -> Digest
GET  /v1/hooks                     -> HookStatus[]
GET  /v1/journal                   ?project&limit -> JournalPage
POST /v1/merge/plan                {project} -> MergePlan
POST /v1/merge/apply               {plan_id} -> MergeResult
POST /v1/merge/rollback            {plan_id} -> MergeResult
POST /v1/duplicates/scan           {} -> DuplicateReport
POST /v1/consolidate               {project, apply} -> ConsolidateReport
```

## 7. Ключевые алгоритмы

### 7.1 Ранжирование
```
norm_bm25 = bm25 / (bm25 + median_bm25_of_matches)
score = 0.55*norm_bm25 + 0.20*recency + 0.15*pin + 0.05*importance + 0.05*trust
```
`recency = exp(-age_days / 90)`, `trust ∈ {1.0 verified, 0.6 stated, 0.3 inferred}`.
`--explain` печатает абсолютный вклад и share.

### 7.2 Пакет контекста
1. state (полный, ≤25% бюджета)
2. latest handoff (≤15%)
3. query-layer: decisions → attempts → notes (≥45%, заимствует)
4. relations/links (остаток)
Правила: слой может занять чужой неиспользованный бюджет; при переполнении
печатается `… N more dropped`; резерв 10% на заголовки.

### 7.3 Резолвинг проекта
Порядок: точный ID → уникальный alias → существующий путь (самый длинный
префикс) → ошибка. Неоднозначность (2 кандидата) — ошибка со списком.
Путь, которого нет на диске, не резолвится.

### 7.4 Sync-guard
Один файл `.brain/sync.lock` с `{pid, host, started_at, ttl}`.
`sync`, `consolidate`, `merge apply` — exclusive; записи записей и хуки —
shared. Exclusive ждёт до 30 c, затем ошибка `sync_busy` с pid/host.

### 7.5 Merge apply
План содержит `{path, sha256, mtime, size}`. Apply: per-file lock →
перечитать fingerprint → при расхождении `stale_plan` (0 записей) →
иначе запись → rollback только при совпадении fingerprint.

## 8. Тест-план

- **Unit:** каждое требование — минимум один тест; таблицы кейсов для
  коллизий, бюджета, ранжирования, merge.
- **Cross-process:** CAS (2 процесса), writer-during-sync, consolidate vs writer,
  journal 2 процесса, sync-lock steal.
- **3-machine harness:** расширить `TestThreeMachinesWriteConcurrently`:
  3 клона × 2 сессии × (note, state, adopt, import) без потерь.
- **Contract:** golden operation set для localapi; паритет OpenAPI ↔ handler.
- **Eval:** 30 реальных запросов с ожидаемыми ID; top-5 ≥ 80%; context содержит
  ≥1 решение/попытку по задаче; отчёт в CI (non-blocking до 0.25.1).
- **Perf:** бенчмарки context/reindex; CLI-тайминги в отчёте.
- **Security:** redaction в edit; tracked-PII guard; SSRF-тест хостов.

## 9. План поставки

| Версия | Содержание | Gate |
|---|---|---|
| **0.24.1** | P0 коннекторы; приватность (GitLab emails, mail.json/state); redaction edit; applied-манифесты; docs P1 | 8/8 коннекторов зелёные; нет tracked PII; тесты |
| **0.25.0** | WS1–WS7 (RET, TMP, PRJ, CON, IMP, HOK, DSK) | eval top-5 ≥ 80%; 3-machine тест; desktop e2e конфликтов |
| **0.25.1** | WS8 (perf) + WS9 (ops/docs) | context ≤ 1 c; pull-reindex ≤ 4 c; публикация без ручных шагов |

## 10. Риски

| Риск | Митигация |
|---|---|
| Нормализация BM25 меняет привычный порядок | Eval-гейт, `--explain`, feature-флаг `ranking.v2` на один релиз |
| Sync-guard сериализует записи и замедляет агентов | Shared-режим, таймаут 30 c, метрика ожиданий |
| Импорт с abort блокирует пользователя | `--skip-conflicts`, dry-run, понятный отчёт |
| Новые API расширяют «замороженный» контракт | Аддитивно, golden set, semver minor |
| Дефолт «require scope» ломает существующих агентов | Предупреждение в 0.25, ошибка в 0.26 |
| Mac недоступен для проверки | Явные критерии на машине; handoff-инструкция |

## 11. Открытые вопросы

1. Включать ли `ranking.v2` флагом по умолчанию в 0.25.0 или после 2 недель
   dogfooding?
2. Делать ли `as-of` снимки state/findings (дороже) или ограничиться явной
   пометкой «current» (дешевле)?
3. Server mode: добивать до canonical или официально объявить append/read-only?
4. Нужны ли prompts в git-бандле при текущей политике локальности?
5. Кто и когда проверяет Mac (SSH закрыт) — блокер для приёмки PRJ-8?
