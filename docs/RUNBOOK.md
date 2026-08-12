# RUNBOOK

Эксплуатация шлюза. Аудитория — владелец. Всё, что здесь описано, выполняется
на VPS, если не сказано иначе.

- [Развернуть с нуля](#развернуть-с-нуля)
- [Добавить потребителя](#добавить-потребителя)
- [Добавить алиас](#добавить-алиас)
- [Добавить провайдера](#добавить-провайдера)
- [Ротировать вендорский ключ](#ротировать-вендорский-ключ)
- [Обновить LiteLLM](#обновить-litellm)
- [Бэкап и восстановление](#бэкап-и-восстановление)
- [Разбор инцидента](#разбор-инцидента)
- [Что проверять после любого изменения](#что-проверять-после-любого-изменения)
- [Известные места, требующие проверки на вашей версии LiteLLM](#известные-места-требующие-проверки-на-вашей-версии-litellm)

---

## Развернуть с нуля

Требования: Docker с плагином compose, Go 1.23+ (только чтобы собрать `gwctl`;
можно собрать на рабочей машине и скопировать бинарник), домен, A-запись
которого указывает на VPS.

```bash
git clone https://github.com/romandots/llm-gateway.git
cd llm-gateway

# 1. Секреты
cp deploy/.env.example deploy/.env
vim deploy/.env
#    GATEWAY_DOMAIN     — домен, на который Caddy получит сертификат
#    LITELLM_MASTER_KEY — openssl rand -hex 32
#    POSTGRES_PASSWORD  — openssl rand -hex 24
#    ANTHROPIC_API_KEY, OPENAI_API_KEY — из консолей вендоров

# 2. Конфиг
make validate            # схема, ссылки, отсутствие секретов в git

# 3. Стек
make up                  # рендерит deploy/litellm/config.yaml и поднимает 4 контейнера
make ps

# 4. Модели и настройки в прокси
make apply

# 5. Проверка
make health              # должно быть всё зелёное
```

Порты наружу торчат только у Caddy (80/443). Postgres и Redis не публикуют
портов вовсе, прокси доступен только изнутри compose-сети.

Дальше — ключи потребителям:

```bash
make key-issue CONSUMER=tansultant-reactivation
make smoke               # прогон контракта живыми запросами
```

Внешний аптайм-мониторинг вешается на `https://<домен>/health/liveness` — этот
endpoint публичный и не требует ключа.

---

## Добавить потребителя

```bash
vim config/consumers.yaml     # имя, owner, whitelist алиасов, бюджет, лимиты
make validate
make apply-dry
make apply
make key-issue CONSUMER=new-project
git add config/consumers.yaml && git commit -m "grant new-project access"
```

Ключ печатается один раз. Не сохраняйте его в файл на VPS — сразу в секреты
проекта-потребителя.

Whitelist давайте по минимуму: потребителю, которому нужен `cheap-fast`, не
нужен `smart`. Расширить всегда можно правкой конфига и `make apply`, без
перевыпуска ключа.

---

## Добавить алиас

Добавление алиаса — **MINOR**: старые потребители ничего не замечают.
Переименование или удаление — **MAJOR**, так делать нельзя без предупреждения
всех потребителей.

```bash
vim config/models.yaml
```

```yaml
  new-alias:
    description: "Зачем он нужен, одной фразой"
    mode: chat
    capabilities:
      streaming: true          # обязательно true для chat
      tools: true              # обязательно true для chat
      json_schema: native      # native | emulated | unsupported — честно
      vision: false
      embeddings: false
    context_window: 200000
    max_output_tokens: 64000
    targets:
      - provider: anthropic
        model: claude-sonnet-5
      - provider: openai       # порядок = приоритет, второй это fallback
        model: gpt-5
```

```bash
make validate
make apply                    # создаст деплойменты и цепочку fallback
make restart                  # цепочки fallback читаются прокси при старте
make smoke                    # проверить, что заявленные capabilities правда есть
```

Затем выдайте алиас тем потребителям, кому он нужен, в `consumers.yaml`.

**Про `capabilities`.** Это декларация, а не автоопределение. Если написать
`json_schema: native` там, где провайдер этого не гарантирует, потребитель
будет доверять невалидному ответу. `make smoke` проверяет каждый заявленный
флаг реальным запросом — не пропускайте его.

---

## Добавить провайдера

В первой итерации поддерживаются `anthropic` и `openai`. Добавление третьего:

1. `internal/config/validate.go` — добавить провайдера в список допустимых.
2. `internal/reconcile/desired.go` — добавить строку в `providerKeyEnv` с
   именем переменной окружения для ключа.
3. `deploy/docker-compose.yml` — пробросить эту переменную в контейнер прокси.
4. `deploy/.env.example` — добавить плейсхолдер.
5. `config/models.yaml` — добавить цель нужным алиасам.

```bash
make test && make validate && make apply && make restart && make health
```

---

## Ротировать вендорский ключ

Вендорские ключи живут только в `deploy/.env` и видны только контейнеру прокси.

```bash
vim deploy/.env               # заменить ANTHROPIC_API_KEY / OPENAI_API_KEY
make restart                  # прокси читает переменные при старте
make health                   # провайдеры должны быть зелёными
```

Ключи в конфиге не хранятся: `gwctl` записывает в деплойменты **ссылку**
`os.environ/ANTHROPIC_API_KEY`, а не значение. Поэтому `make apply` после смены
вендорского ключа не нужен.

Старый ключ в вендорской консоли отзывайте после того, как `make health`
показал зелёное на новом.

---

## Обновить LiteLLM

Версия прибита в `deploy/.env` (`LITELLM_VERSION`) — обновление всегда
осознанное.

```bash
make backup                   # сначала дамп: миграции схемы необратимы
vim deploy/.env               # поднять LITELLM_VERSION
make restart
make health
make smoke                    # главное: контракт не изменился
```

Если `make smoke` покраснел — откатите версию в `.env` и `make restart`. При
неудачной миграции БД восстановитесь из дампа (см. ниже).

Отдельно проверьте после обновления пункты из раздела
[«Известные места»](#известные-места-требующие-проверки-на-вашей-версии-litellm):
именно они завязаны на внутренние детали прокси.

---

## Бэкап и восстановление

В Postgres лежит всё состояние: ключи (хэши), бюджеты, spend-логи.

```bash
make backup                   # backups/2026-08-12-1430.sql.gz
```

Ставьте в cron ежедневно и **храните копию вне VPS** — бэкап на том же диске
не защищает от потери диска:

```cron
30 3 * * * cd /opt/llm-gateway && make backup >> /var/log/gw-backup.log 2>&1
```

Восстановление — операция с подтверждением, она затирает текущее состояние:

```bash
make restore FILE=backups/2026-08-12-1430.sql.gz
make restart
make health
make key-list                 # убедиться, что ключи на месте
```

Секретов ключей в дампе нет — только хэши. Восстановление возвращает права и
бюджеты, но не делает ключи «читаемыми»: у потребителей их копии продолжают
работать.

---

## Разбор инцидента

### «Шлюз не отвечает»

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://<домен>/health/liveness
make ps
make logs SERVICE=litellm
make health
```

Порядок сужения: Caddy → прокси → Postgres/Redis → провайдеры. `make health`
проходит по этой же цепочке и останавливается на первом мёртвом звене.

### «У потребителя 401»

Ключ отозван, истёк или это чужой ключ.

```bash
make key-list                 # статус active / deprecated, дата expires
```

Если ключ в `deprecated` и истёк — выпустить новый: `make key-issue`.

### «У потребителя 403»

Алиас не в whitelist ключа. Смотреть `aliases` потребителя в
`config/consumers.yaml`; расширить и `make apply` (перевыпуск ключа не нужен).

### «У потребителя 429»

```bash
make spend BY=consumer SINCE=1d
make key-list                 # колонки BUDGET и SPEND
```

Если `SPEND` упёрся в `BUDGET` — это бюджет, ждать до конца окна или поднимать
лимит в `consumers.yaml`. Если нет — упёрлись в rpm/tpm, снижать параллелизм у
потребителя или поднимать лимиты.

### «Ответы стали хуже / медленнее»

```bash
make spend BY=alias SINCE=1d      # колонка FALLBACKS
make health
```

Ненулевые `FALLBACKS` означают, что запросы обслуживает не основная модель, а
запасная. Причина — в `make health` (мёртвый или лимитирующий провайдер).

### «Нужно понять, что было в конкретном запросе»

Никак. Тела запросов и ответов не логируются намеренно — на VPS не должна
оседать переписка всех проектов. Есть метаданные: `x-gw-request-id`, алиас,
модель, токены, стоимость, статус, факт fallback. Попросите у потребителя
`x-gw-request-id` из ответа.

---

## Что проверять после любого изменения

```bash
make validate     # конфиг
make apply-dry    # что именно изменится
make apply
make restart      # если менялся proxy.yaml или список алиасов
make health       # прокси, БД, Redis, провайдеры
make smoke        # контракт целиком, живыми запросами
```

Идемпотентность — не обещание, а проверка: повторный `make apply` обязан
напечатать `no changes`. Если печатает что-то другое — прокси не принимает
запись, разбирайтесь до того, как уйдёте с терминала.

---

## Известные места, требующие проверки на вашей версии LiteLLM

Data plane взят как есть, и три вещи в контракте опираются на внутренние
детали прокси. Они закрыты smoke-тестом, поэтому расхождение будет видно сразу
— но знать, где смотреть, стоит заранее.

**1. Заголовки `x-gw-*`.** `deploy/Caddyfile` переименовывает заголовки,
которые отдаёт LiteLLM (`x-litellm-model-id`, `x-litellm-model-group`,
`x-litellm-attempted-fallbacks`), в контрактные `x-gw-model`, `x-gw-alias`,
`x-gw-fallback`. `x-gw-request-id` Caddy генерирует сам. Если после обновления
smoke-тест сообщает `response header x-gw-model is missing` — посмотрите
фактические заголовки ответа и поправьте правила `header_down >… …` в
`deploy/Caddyfile`:

```bash
curl -sSD - -o /dev/null https://<домен>/v1/chat/completions \
  -H "Authorization: Bearer $GW_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"balanced","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}' \
  | grep -i '^x-'
```

**2. Формат тел ошибок.** `deploy/litellm/gw_hooks.py` формирует контрактные
`unknown_model_alias`, `alias_not_permitted` и `budget_exceeded` с полями
`available` / `retry_after` / `budget`. Прокси может обернуть тело исключения
в свою оболочку — smoke-тест принимает и `.error.code`, и `.detail.error.code`.
Если поменяется и это, правьте `_check_alias` / `_check_budget` в хуке;
перезапуск: `make restart` (файл монтируется, пересборка образа не нужна).

**3. Fallback между провайдерами.** Порядок `targets` превращается в отдельные
модель-группы `<alias>--fallback-N` и цепочку `router_settings.fallbacks` в
сгенерированном `deploy/litellm/config.yaml`. Эти группы намеренно **не**
входят в whitelist ключей: они внутренние. Проверка — акцептанс-критерий 11:

```bash
# временно сломать основную цель алиаса (например, опечатка в модели), затем
SMOKE_FALLBACK=1 make smoke
```

Ожидаемое поведение: `200` и `x-gw-fallback: true`. Если вместо этого приходит
ошибка о недоступном алиасе — LiteLLM проверяет права ключа и на fallback-цели;
в этом случае добавьте fallback-группы в `models` ключа (в
`internal/reconcile/desired.go`, функция `DesiredKeyFor`) и перечитайте влияние
на принцип наименьших привилегий.
