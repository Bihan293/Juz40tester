# Аудит архитектуры (2026-10-01)

Цель: проверить, можно ли поверх текущего кода спокойно делать **админ-меню** и **подписки**.

## Что проверено

| Проверка | Результат |
|---|---|
| `go build ./...`, `go vet ./...`, `gofmt`, `staticcheck` | ✅ чисто, 0 замечаний |
| Юнит-тесты `go test -race ./...` | ✅ все проходят, гонок данных нет |
| Интеграционные тесты на реальном PostgreSQL 17 | ✅ проходят (streak, лидерборды, генерация с ремонтом, sweep, статистика) |
| Все миграции (сейчас 13) на чистой базе | ✅ применяются |
| Ручное ревью: `cmd/bot`, `handlers`, `services/quiz`, `repositories/*`, `database`, `bot`, воркер генерации | см. ниже |

## Что в порядке (надёжно)

- **Слои разделены правильно**: `handlers` (UI) → `services` (логика) → `repositories` (SQL). Новые функции встраиваются без переделки.
- **Ответ на вопрос** — одна транзакция с `FOR UPDATE` + guard `answered = FALSE`: двойной тап не засчитывается дважды.
- **Владелец попытки проверяется** во всех запросах (`WHERE id = $1 AND user_id = $2`).
- **Очередь генерации**: `FOR UPDATE SKIP LOCKED`, heartbeat, возврат зависших задач, ретраи, recover от паник — безопасно и при рестартах.
- **Webhook**: проверка `WEBHOOK_SECRET` (constant-time), лимит тела 1 МБ, таймаут на апдейт, recover в обработчике.
- **SQL-инъекций нет**: везде параметры `$1..$n`.
- Секреты только из env, в Docker приложение работает не от root.

## Исправлено в этом аудите

1. **Гонка миграций при деплое.** При zero-downtime деплое (Render) старый и новый инстанс стартуют одновременно → оба применяли одну миграцию → один падал на `schema_migrations_pkey`. Воспроизведено тестом (4 параллельных запуска — FAIL). Теперь миграции сериализуются через `pg_advisory_lock`; тест `internal/database/migrate_integration_test.go` проходит.
2. **Бот в группе.** Если бота добавить в группу, он отвечал главным меню на каждое сообщение и регистрировал всех участников. Теперь сообщения не из личных чатов игнорируются.

## Мелочи, не мешающие (можно не трогать)

- `handlers.go` большой (~1600 строк), роутер — один `switch`. Не баг, но **новые фичи кладите в отдельные файлы** (`handlers/admin.go`, `handlers/subscription.go`).
- Наблюдатели «⏳ тест генерируется» живут в памяти: после рестарта сообщение не обновится само (тест при этом генерируется, по кнопке откроется).

## Как добавлять админку и подписки (чтобы не сломать)

**Админ-меню**
- Список админов — env `ADMIN_IDS` (Telegram ID через запятую) в `config`. Проверка `isAdmin(from.ID)` **в каждом** админ-обработчике, а не только при показе кнопки (callback можно подделать).
- Свой префикс callback-ов `adm:` и ветка в `handleCallback` **до** `default`; команда `/admin` в `handleMessage`.
- Код — в `internal/handlers/admin.go`, SQL — в `internal/repositories/admin.go`.

**Подписки (Telegram Stars, валюта `XTR`)**
- В `bot.Update` добавить `PreCheckoutQuery` и `Message.SuccessfulPayment`; в `SetWebhook` → `allowed_updates` добавить `"pre_checkout_query"` (сейчас там только `message` и `callback_query` — **без этого оплата не дойдёт**).
- На `pre_checkout_query` ответить `answerPreCheckoutQuery` за ≤10 секунд — обрабатывать синхронно и быстро.
- Новая миграция `000014_subscriptions.up.sql` (номера `000012` = `topic_stats` и `000013` = `attempts_unique_and_quality_postpone` уже заняты; брать следующий свободный номер на момент добавления): таблица `subscriptions(user_id, plan, starts_at, expires_at, telegram_payment_charge_id UNIQUE)` — UNIQUE защищает от двойного зачисления платежа.
- Проверка «есть ли подписка» — один метод в `services`, вызывать в `openTest` / `openWeakSubject` (там уже есть точки, где решается «можно ли открыть»).
- Обработчик сообщений сейчас пропускает сообщения без текста в `default` → главное меню; `successful_payment` нужно обработать **раньше** этого `switch`.

## Вывод

Архитектура рабочая и не хрупкая: критичные места (ответы, очередь, миграции, webhook) защищены транзакциями и тестами. Можно смело добавлять админку и подписки по плану выше.

## Часть 3 — надёжность и инфраструктура (#19–#31)

| # | Что изменено | Тест |
|---|---|---|
| 19 | `cmd/bot/webhook.go`: `updateDispatcher` с `sync.WaitGroup`; при SIGTERM воркер отменяется, HTTP-сервер останавливается, апдейты в обработке дожидаются (таймаут 20 с, потом отмена контекста). Новые апдейты во время выключения → 503 (Telegram доставит повторно). Прерванная задача генерации → `ReleaseJob`: `pending`, `not_before=now()`, `attempts` не растёт | `TestWebhookShutdownWaitsForUpdates`, `TestWebhookShutdownTimeoutCancelsHandlers`, `TestShutdownReleasesRunningJob` (PG) |
| 20 | Семафор на 32 одновременных апдейта; пул pgx через `pgxpool.ParseConfig`, `MaxConns = DB_MAX_CONNS` (по умолчанию 15) | `TestWebhookConcurrencyBounded`, `TestDBMaxConns` |
| 21 | Миграции идут через отдельное прямое подключение: `MIGRATION_DATABASE_URL` или `DATABASE_URL` с убранным `-pooler` у хоста Neon (`database.DirectURL`, `MigrateURL`) | `TestDirectURL`, `TestMigrateConcurrent` (PG) |
| 22 | `setWebhook` после старта HTTP, в фоне, с повтором и экспоненциальной паузой (2 с → 1 мин) вместо `log.Fatalf` | `TestSetWebhookRetries`, `TestSetWebhookStopsOnShutdown`, ручной запуск бинаря |
| 23 | `RunQualitySweep` обёрнут в `pg_try_advisory_xact_lock` (`TryQualitySweepLock`); если lock занят — sweep пропускается | `TestQualitySweepSingleInstance` (PG) |
| 24 | Backfill запускается в фоне после старта HTTP; история читается потоком, агрегируется по темам, запись — батчами upsert; `LOCK TABLE … EXCLUSIVE` + перезапись, а не прибавление, поэтому параллельные живые ответы не считаются дважды | `TestWeakTopicsBackfillMatchesLive`, `TestBackfillAfterLiveAnswersNoDoubleCount` (PG) |
| 25 | Все `$n::interval` с Go-строкой заменены на `make_interval(secs => $n)` + `int(d.Seconds())` (`ResetStuckRunningJobs`, `FailJob`, `PostponeQualityCheck`, `AbandonStaleAttempts`) | `TestFailJobBackoffInterval`, `TestResetStuckRunningJobsRespectsAttempts`, `TestAttemptGuards`, `TestQualitySweepRepository` (PG) |
| 26 | Обработчик зависших задач: если `attempts >= maxJobAttempts`, задача → `failed`; флаг `urgent` больше не выставляется всем подряд | `TestResetStuckRunningJobsRespectsAttempts` (PG) |
| 27 | `DeletePersonalTest`: сначала берутся `question_id` удаляемого теста, удаляются только они и только если на них больше нет ссылок; всё в одной транзакции | `TestDeletePersonalTestOnlyOwnOrphans`, `TestFlowWeakTopicsSharedClone` (PG) |
| 28 | `config.Load`: в production (`APP_ENV` не dev/local/test) без `WEBHOOK_SECRET` бот не запускается | `TestWebhookSecretRequiredInProduction`, ручной запуск бинаря |
| 29 | Сломанный JSON апдейта логируется, Telegram получает 200 | `TestWebhookMalformedJSONReturns200`, ручной запуск бинаря |
| 30 | `bot.Client.call`: на 429 читается `parameters.retry_after`, не больше 2 повторов, ожидание не дольше 30 с, иначе `RateLimitError` | `TestCall429RetriedAfterRetryAfter`, `TestCall429BoundedRetries`, `TestCall429TooLongWaitFailsFast`, `TestCallNon429ErrorNotRetried` |
| 31 | Оставлен вариант в памяти; поведение после рестарта и при 429 проверено и описано в `docs/GROQ_LIMITS.md` | `TestLimiterAfterRestart`, `TestClient429PerDayBlocksForAnHour` |

Повторная проверка: #13 — `TestReviveChainTestLockedIsRefusedAtServiceLevel` (у `GeneratorService.reviveChainTest` убран экспорт, единственный вход — `QuizService.ReviveChainTest` с проверкой `unlockedMax`); #16 — `TestReplaceQuestionContentAtomic` (ошибка подставлена триггером, после отката нет ни нового текста, ни потерянного прогресса); #18 — `TestEnsurePersonalTestKeepsActiveAttemptAndNormalizes`.

## Часть 5 — последние пункты (#37–#41) и финальная проверка

| # | Что изменено | Тест / проверка |
|---|---|---|
| 37 | `GetSubjectScreen`: нижняя граница видимого диапазона — максимальный `TestNumber` цепочки (`maxChainNumber`), а не `len(chain)`. При дырах 1,2,3,7,8 тесты 7 и 8 больше не скрываются | `TestMaxChainNumberWithGaps`, `TestSubjectScreenChainGaps` (PG; со старым кодом падает: `MaxVisible = 5, want 8`) |
| 38 | `handleMessage` маршрутизирует по `commandKey(text)`: `/start payload`, `/start@Bot`, `/start@Bot payload` → `/start`; то же для остальных команд; тексты кнопок не меняются, `/started` ≠ `/start` | `TestCommandKey` |
| 39 | **Ограничение, не баг.** `validateChainDifficulty` сравнивает среднюю *самооценку* модели с целью позиции. Это ловит только явное игнорирование инструкции. Простой надёжной независимой проверки сложности нет, поэтому код не менялся; ограничение описано в README | — |
| 40 | `TranslatorService.inFlight`: запись со счётчиком ссылок; `lockTest` возвращает `release`, который вызывается через `defer` (значит, и при ошибке, и при panic), и удаляет ключ, когда его никто не держит и не ждёт | `TestTranslatorInFlightCleanup` (1000 ключей → 0, panic → 0, 64 горутины → взаимоисключение по ключу, map пуст), `go test -race` |
| 41 | Миграции подписок в коде нет (подписки не реализованы) — новый файл не создавался. В плане выше номер исправлен на следующий свободный (`000014`). `database.Migrate` теперь падает при двух файлах с одним номером (раньше один молча пропускался) | `TestMigrationVersionsUniqueAndContiguous`, `TestMigrateConcurrent` (PG) |

Финальная проверка:
- **quality.go**: найдены false positive, из-за которых зря запускался бы платный repair: «(ответ дайте в м/с)», «Выберите правильный ответ: …», орфография «по-моему / по моему / помоему», идентификаторы `my_var` в вопросах по программированию. Исправлено (`stemLeaksAnswer`, `isSpellingQuestion`, `isCodeQuestion`). Реальные утечки («? Ответ: B», «(ответ: 4)», «Подсказка: …», помеченный вариант, «все ответы верны», выделяющийся заполненный пропуск/запятая) по-прежнему ловятся — `TestQualityDetectorsEdgeCases`.
- **API моделей** (сверено с документацией 2026-10-02, без реальных ключей): Groq `qwen/qwen3.8-27b` принимает `reasoning_effort: "none"`; `openai/gpt-oss-120b` — `low/medium/high`. DeepSeek: модели `deepseek-flash` и `deepseek-v4-pro`, `thinking: {"type":"enabled"}` + `reasoning_effort` `low/high`. Код отправляет именно это. Замечание: в DeepSeek thinking включён по умолчанию. Запросы без поля `thinking` (шаги 2–3 `jsonWithFallback`) отправляются только после того, как endpoint (прокси) отклонил thinking-параметры: явное `{"type":"disabled"}` там тоже отклонили бы, поэтому код не менялся. На официальном API этот путь не используется. Реальные запросы с ключом **не выполнялись**: принимает ли API параметры, проверено только по документации и fake-серверам в тестах.
- **Существующие тесты**: тестов без проверок нет; критичный SQL (ответы, очередь, удаление, миграции, sweep, статистика) покрыт интеграционными тестами на PG.
- `go build`, `go vet`, `go test ./...`, `go test -race ./...`: чисто. PostgreSQL 17.11: на чистой базе применены 13 миграций без дублей; на базе с 000001–000012 (схема прошлого релиза) применилась только 000013; повторный запуск ничего не применяет.
