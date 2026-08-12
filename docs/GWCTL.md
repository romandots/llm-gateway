# `gwctl` — справочник

Control plane шлюза: читает декларативный конфиг из `config/` и приводит
состояние LiteLLM-прокси к описанному. Аудитория — владелец шлюза.

- [Обзор](#обзор)
- [Конфигурация](#конфигурация)
- [Глобальные флаги](#глобальные-флаги)
- [Команды](#команды)
- [Сценарии](#сценарии)
- [Troubleshooting](#troubleshooting)

---

## Обзор

```
gwctl <command> [subcommand] [flags]
```

Три принципа, из которых следует всё остальное поведение:

1. **Конфиг — источник правды.** `gwctl` приводит прокси к конфигу, а не
   наоборот. Изменение, сделанное руками в admin UI, будет откачено ближайшим
   `apply`.
2. **Reconcile идемпотентен.** `gwctl apply` дважды подряд — второй раз печатает
   `no changes`. Это проверяется тестом, а не обещанием.
3. **`apply` не трогает секреты.** Он создаёт и обновляет деплойменты и
   атрибуты ключей (алиасы, бюджеты, лимиты), но никогда не печатает и не
   хранит значения ключей. Секрет показывается один раз — в `key issue` и
   `key rotate`.

Отдельный статический бинарник, а не скрипт внутри образа прокси: обновление
LiteLLM не должно ломать управление ключами. Один и тот же файл кладётся и на
VPS, и на рабочую машину.

### Сборка и установка

```bash
make build            # bin/gwctl
make install          # в $(go env GOPATH)/bin
```

---

## Конфигурация

Три файла в `config/`, все под git, **секретов в них нет**.

### `config/models.yaml` — таксономия алиасов

| Поле | Тип | Описание |
|---|---|---|
| `version` | int | Схема файла. Сейчас `1` |
| `aliases.<name>.description` | string | Человекочитаемое назначение. Обязательно |
| `aliases.<name>.mode` | `chat` \| `embedding` | Какой endpoint обслуживает алиас. По умолчанию выводится из `capabilities.embeddings` |
| `aliases.<name>.capabilities.streaming` | bool | Обязательно `true` для chat-алиасов |
| `aliases.<name>.capabilities.tools` | bool | Обязательно `true` для chat-алиасов |
| `aliases.<name>.capabilities.json_schema` | `native` \| `emulated` \| `unsupported` | Статус structured output |
| `aliases.<name>.capabilities.vision` | bool | Гарантия по картинкам |
| `aliases.<name>.capabilities.embeddings` | bool | Только для `mode: embedding` |
| `aliases.<name>.context_window` | int | Размер входного окна, попадает в `GET /v1/models` |
| `aliases.<name>.max_output_tokens` | int | Обязательно для chat, запрещено для embedding |
| `aliases.<name>.targets[]` | list | **Порядок = приоритет.** Первый — основной, остальные — fallback |
| `targets[].provider` | `anthropic` \| `openai` | Провайдер |
| `targets[].model` | string | Вендорский идентификатор модели |
| `targets[].params` | map | Дополнительные параметры LiteLLM (например `thinking`) |

`capabilities` — это **декларация**, а не автоопределение. Расхождение с
реальностью — дефект конфига, и `make smoke` проверяет каждый заявленный флаг
реальным запросом.

### `config/consumers.yaml` — потребители

| Поле | Тип | Описание |
|---|---|---|
| `consumers.<name>.description` | string | Что за проект |
| `consumers.<name>.owner` | string | Кто отвечает. Обязательно |
| `consumers.<name>.aliases` | list | **Whitelist** разрешённых алиасов. Принцип наименьших привилегий |
| `consumers.<name>.budget.amount_usd` | number | Лимит расхода за период |
| `consumers.<name>.budget.period` | `daily` \| `weekly` \| `monthly` | Окно бюджета |
| `consumers.<name>.limits.rpm` | int | Запросов в минуту |
| `consumers.<name>.limits.tpm` | int | Токенов в минуту |

Имя потребителя становится частью ключа (`sk-gw-<consumer>-<random>`), поэтому
допускается только нижний регистр и дефисы.

### `config/proxy.yaml` — поведение прокси

| Поле | Описание |
|---|---|
| `request.timeout_seconds` | Таймаут. Больше 30 не принимается: контракт обещает потребителю не больше 30 s |
| `request.num_retries` | Внутренние ретраи до перехода на fallback |
| `request.retry_after_seconds` | Пауза между ретраями |
| `fallback.enabled` | Включает цепочки fallback, выведенные из порядка `targets` |
| `fallback.trigger_on` | Коды-триггеры. `400` запрещён: ошибка в самом запросе детерминирована |
| `logging.request_bodies` | **Держать `false`.** Иначе на VPS осядет переписка всех проектов |
| `logging.response_bodies` | То же |
| `logging.metadata` | Метаданные для учёта расходов. Без них `gwctl spend` пуст |
| `cache.enabled` | Кэш ответов в Redis |

`gwctl apply` рендерит из `proxy.yaml` файл `deploy/litellm/config.yaml`. Он
**генерируемый**, правится только через `proxy.yaml`, лежит в git и применяется
после перезапуска прокси (`make restart`).

---

## Глобальные флаги

| Флаг | По умолчанию | Описание |
|---|---|---|
| `--root` | `.` | Корень репозитория с `config/` и `deploy/` |
| `-c, --config` | `<root>/config` | Каталог с тремя YAML-файлами |
| `--endpoint` | `http://localhost:4000` | Адрес прокси. Env: `GATEWAY_ENDPOINT` |
| `--master-key` | — | Мастер-ключ LiteLLM. Env: `LITELLM_MASTER_KEY`, либо `deploy/.env` |
| `-o, --output` | `table` | `table` для человека, `json` для скриптов |
| `--timeout` | `1m0s` | Таймаут обращений к прокси |
| `-y, --yes` | `false` | Не спрашивать подтверждений (для make и CI) |

Мастер-ключ ищется по порядку: флаг → переменная окружения → `deploy/.env`.
На VPS это значит, что экспортировать ничего не нужно.

---

## Команды

### `gwctl validate`

Проверяет конфиг **локально, без сети**. Ловит:

* неизвестный алиас в `consumers.yaml`;
* дубликат потребителя или алиаса (дублирующийся YAML-ключ — ошибка, а не
  «побеждает последний»);
* опечатку в имени поля (строгий парсинг);
* отсутствующий или пустой `targets`;
* невалидный период бюджета;
* таймаут выше обещанных контрактом 30 s;
* `400` в списке `fallback.trigger_on`;
* вендорское имя в имени алиаса;
* секрет, попавший в `config/*.yaml`, и `.env`, не закрытый `.gitignore`.

```
$ gwctl validate
ok: 8 aliases, 2 consumers, no problems found
```

```
$ gwctl validate
SEVERITY   FILE             PATH                             MESSAGE
error      consumers.yaml   consumers.my-bot.aliases[1]      unknown alias "smrt", not defined in models.yaml
warning    consumers.yaml   consumers.my-bot.limits.tpm      no token rate limit set
error: configuration is invalid
```

Возвращает ненулевой код выхода при наличии `error`. Предупреждения его не
меняют.

### `gwctl diff`

Показывает расхождение конфига и прокси, ничего не меняя. То же, что
`apply --dry-run`, но без вопроса в конце.

```
$ gwctl diff
update   model.update   balanced
             litellm_params.model: anthropic/claude-haiku-4-5 -> anthropic/claude-sonnet-5
missing  key.missing    my-telegram-bot
             no key issued yet; run: gwctl key issue my-telegram-bot

note: key of consumer "old-project" has no entry in consumers.yaml; revoke it with: gwctl key revoke old-project
```

### `gwctl apply`

Приводит прокси к конфигу.

| Флаг | Описание |
|---|---|
| `--dry-run` | Напечатать план и выйти, ничего не меняя |
| `--prune-keys` | Отозвать ключи потребителей, удалённых из `consumers.yaml`. По умолчанию выключено: потерянный ключ не восстановить |
| `--render-only` | Только перегенерировать `deploy/litellm/config.yaml`, не обращаясь к прокси. Нужно для первого запуска стека |

```
$ gwctl apply --dry-run
create   model.create   balanced (anthropic/claude-sonnet-5)
create   model.create   balanced--fallback-1 (openai/gpt-5)
missing  key.missing    my-telegram-bot
             no key issued yet; run: gwctl key issue my-telegram-bot

dry run: nothing was changed

$ gwctl apply
...
Apply 2 change(s) to https://gw.example.com? [y/N]: y

applied 2 change(s)

$ gwctl apply
no changes
```

Чего `apply` **не** делает:

* не выпускает ключи. Консьюмер без ключа показывается строкой `key.missing` —
  секрет можно показать только один раз, поэтому это явная операция;
* не удаляет деплойменты, созданные руками в admin UI. Про них печатается
  предупреждение;
* не отзывает ключи без `--prune-keys`.

### `gwctl key issue <consumer>`

Выпускает ключ и печатает секрет **один раз**.

```
$ gwctl key issue my-telegram-bot
consumer: my-telegram-bot
aliases:  cheap-fast
key:      sk-gw-my-telegram-bot-8sKq2mNpX4vRtYuJhGfDcVbNmQwErTyUiOpAsDfG

This is the only time the key is shown. Store it in the consumer's secret store now.
```

С `--output json` — машинно читаемо, для передачи в менеджер секретов:

```
$ gwctl key issue my-telegram-bot --output json | jq -r .key
```

Если у потребителя уже есть активный ключ, команда откажет и предложит
`key rotate`.

### `gwctl key list`

```
$ gwctl key list
CONSUMER                  KEY           ALIASES               BUDGET     SPEND   LIMITS                STATUS       ISSUED             EXPIRES
my-telegram-bot           sk-gw-…AsDfG  cheap-fast            1.00/1d    0.34    30 rpm / 50000 tpm    active       2026-08-12 09:15   -
tansultant-reactivation   sk-gw-…Kj4Ha  balanced,cheap-fast   5.00/1d    4.12    60 rpm / 100000 tpm   active       2026-08-01 11:02   -
tansultant-reactivation   sk-gw-…9dQw1  balanced,cheap-fast   5.00/1d    1.80    60 rpm / 100000 tpm   deprecated   2026-07-14 08:30   2026-08-13 11:02
```

Секрет не показывается никогда — только маска. Ключи, созданные вручную в
admin UI, в списке не появляются: `gwctl` показывает только то, чем управляет.

### `gwctl key revoke <consumer>`

| Флаг | По умолчанию | Описание |
|---|---|---|
| `--grace` | `0` | Сколько ещё живёт старый ключ. `0` — отзыв немедленный |

```
$ gwctl key revoke my-telegram-bot
Revoke the key of "my-telegram-bot" immediately? Requests will start failing with 401. [y/N]: y
key of my-telegram-bot revoked

$ gwctl key revoke my-telegram-bot --grace 24h --yes
key of my-telegram-bot marked deprecated, stops working in 24h
```

При `--grace` ключ помечается deprecated, ему проставляется срок истечения, а
его alias освобождается — чтобы новый ключ тому же потребителю можно было
выпустить, пока старый ещё работает.

### `gwctl key rotate <consumer>`

Композиция `revoke --grace` и `issue` поверх штатного механизма истечения
ключей в LiteLLM. Отдельной машинерии за этим нет.

| Флаг | По умолчанию | Описание |
|---|---|---|
| `--grace` | `24h` | Сколько ещё работает предыдущий ключ |

```
$ gwctl key rotate tansultant-reactivation --grace 24h
Rotate the key of "tansultant-reactivation"? The old key stops working in 24h. [y/N]: y
consumer: tansultant-reactivation
aliases:  cheap-fast, balanced
key:      sk-gw-tansultant-reactivation-Lm3PqZx…

This is the only time the key is shown. Store it in the consumer's secret store now.
the previous key keeps working for 24h
```

Старый ключ выводится из строя **первым**: если выпуск нового упадёт,
потребитель останется с работающим ключом, а не без ключа вовсе.

### `gwctl spend`

| Флаг | По умолчанию | Описание |
|---|---|---|
| `--by` | `consumer` | Группировка: `consumer`, `alias`, `model` |
| `--since` | `7d` | Окно: `30m`, `12h`, `7d`, `4w` |

```
$ gwctl spend --by consumer --since 7d
CONSUMER                  REQUESTS   TOKENS IN   TOKENS OUT   COST USD   FALLBACKS
tansultant-reactivation   12403      8.2M        1.1M         18.42      3
my-telegram-bot           891        0.4M        0.2M         2.11       0
TOTAL                     13294      8.6M        1.3M         20.53      3
```

Строки отсортированы по стоимости: отчёт существует, чтобы искать, куда ушли
деньги. Колонка `FALLBACKS` считает запросы, которые обслужил не основной
провайдер — ненулевое значение означает, что основной проваливался.

Группировка `--by alias` относит запросы, ушедшие через fallback, к тому
алиасу, который просил потребитель, а не к внутренней fallback-группе.

### `gwctl models`

| Флаг | Описание |
|---|---|
| `--local` | Только конфиг, без обращения к прокси |

```
$ gwctl models
ALIAS           MODE        PRIMARY                         FALLBACKS           CAPABILITIES                      PROXY
balanced        chat        anthropic/claude-sonnet-5       openai/gpt-5        stream,tools,json:native          in sync
cheap-fast      chat        anthropic/claude-haiku-4-5      openai/gpt-5-mini   stream,tools,json:emulated        in sync
embed-fast      embedding   openai/text-embedding-3-small   -                   embeddings                        in sync
reasoning       chat        anthropic/claude-opus-5         -                   stream,tools,json:native          1/1 target(s) missing in proxy
```

Колонка `PROXY` — сверка с живым прокси: `in sync` или сколько целей ещё не
применено. `not checked` означает `--local`.

### `gwctl health`

```
$ gwctl health
COMPONENT                        STATUS   DETAIL
proxy                            ok       liveness
postgres                         ok       connected
redis                            ok       connected
readiness                        ok       connected
model anthropic/claude-sonnet-5  ok
model openai/gpt-5               failed   AuthenticationError: Incorrect API key provided
```

Ненулевой код выхода, если хоть одна проверка провалилась — годится для
внешнего мониторинга. Если прокси не отвечает вовсе, остальные проверки не
выполняются: через мёртвый прокси проверять нечего.

### `gwctl version`

```
$ gwctl version
COMPONENT   VERSION
gwctl       v1.0.0
```

---

## Сценарии

### Выдать доступ новому проекту

```bash
# 1. Описать потребителя в конфиге
vim config/consumers.yaml

# 2. Проверить локально
make validate

# 3. Посмотреть, что изменится
make apply-dry

# 4. Применить
make apply

# 5. Выпустить ключ и сразу положить его в секреты проекта
make key-issue CONSUMER=new-project

# 6. Зафиксировать в истории, кто когда получил доступ
git add config/consumers.yaml && git commit -m "grant new-project access to cheap-fast"
```

Git здесь — не формальность: он бесплатно даёт журнал «кто когда получил
доступ к чему».

### Расследовать перерасход

```bash
# Кто потратил
make spend BY=consumer SINCE=7d

# На каких классах задач
make spend BY=alias SINCE=7d

# Какие вендорские модели за этим стоят
make spend BY=model SINCE=7d

# Не сыпется ли основной провайдер (колонка FALLBACKS) и жив ли он сейчас
make health
```

Дальше — либо поднять бюджет в `consumers.yaml`, либо перевести потребителя на
более дешёвый алиас, либо чинить провайдера. Первые два — правка конфига и
`make apply`.

### Ротировать ключ потребителя

```bash
make key-rotate CONSUMER=tansultant-reactivation GRACE=24h
# передать новый ключ в проект, дождаться выкатки
make key-list                       # убедиться, что старый ушёл в deprecated
```

Через сутки старый ключ истечёт сам. Если нужно быстрее:

```bash
make key-revoke CONSUMER=tansultant-reactivation GRACE=0
```

### Сменить модель под алиасом

```bash
vim config/models.yaml              # поменять targets[0].model
make validate && make apply
make smoke                          # подтвердить, что заявленные capabilities не сломались
```

Для потребителей это **PATCH**: контракт не изменился, никто ничего не заметил.

---

## Troubleshooting

**`no master key: pass --master-key, set LITELLM_MASTER_KEY, or put it in deploy/.env`**
`gwctl` ищет ключ в флаге, переменной окружения и `deploy/.env` относительно
`--root`. Если запускаете не из корня репозитория — передайте `--root`.

**`configuration is invalid; run gwctl validate for the full report`**
Команды, которые ходят в прокси, отказываются работать на невалидном конфиге:
разлить наполовину сломанное состояние в живой шлюз хуже, чем остановиться.

**`gwctl apply` каждый раз показывает одни и те же изменения**
Значит, прокси не принимает запись. Проверьте, что у прокси включён
`store_model_in_db` (он в сгенерированном `deploy/litellm/config.yaml`) и что
мастер-ключ действительно мастер-ключ, а не ключ потребителя.

**`gwctl spend` пустой после реальных запросов**
Проверьте `logging.metadata: true` в `config/proxy.yaml` и перезапустите прокси
после `apply` — настройки прокси подхватываются только при старте.

**`consumer already has an active key`**
Так и задумано. Нужен новый ключ без простоя — `gwctl key rotate`.

**Изменения в `proxy.yaml` не действуют**
`gwctl apply` перезаписывает `deploy/litellm/config.yaml`, но прокси читает его
при старте. Нужен `make restart`; `apply` про это печатает note.
