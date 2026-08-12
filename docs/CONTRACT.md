# Контракт LLM Gateway

Документ для **разработчика-потребителя**: всё, что нужно, чтобы подключить
свой сервис к шлюзу. Про внутреннее устройство здесь нет ничего — и не должно
быть: оно меняется, контракт нет.

---

## 1. Что это

Одна HTTP-точка входа для всех ИИ-интеграций. Вы просите **класс модели**
(«дёшево-быстро», «умная»), а не конкретную вендорскую модель. Какая реальная
модель обслуживает класс сегодня — решает шлюз. Смена модели под алиасом для вас
незаметна и не требует изменений в вашем коде.

Формат запроса и ответа — **OpenAI-совместимый**. Подойдёт любой OpenAI SDK:
достаточно поменять `base_url` и `api_key`.

```
base_url: https://<домен-шлюза>/v1
api_key:  sk-gw-<ваш-проект>-<...>
```

---

## 2. Аутентификация

```
Authorization: Bearer sk-gw-<consumer>-<random>
```

Ключ выдаёт владелец шлюза, **один раз** — при выпуске. Восстановить его
невозможно, только выпустить новый.

* Ключ — секрет. Он не должен попадать в git, в логи, в фронтенд.
* В ключе есть имя вашего проекта. Если он утечёт в чужой лог, владелец сразу
  поймёт, чей он и что отзывать.
* Каждому ключу разрешён **свой список алиасов**. Обращение к неразрешённому
  алиасу — `403`, а не тихая подмена.

---

## 3. Endpoints

| Метод | Путь | Назначение |
|---|---|---|
| `POST` | `/v1/chat/completions` | Чат: streaming, tools, structured output, vision |
| `POST` | `/v1/embeddings` | Эмбеддинги |
| `GET` | `/v1/models` | Алиасы, доступные вашему ключу, с матрицей возможностей |
| `GET` | `/health/liveness` | Жив ли шлюз (без аутентификации) |
| `GET` | `/health/readiness` | Готов ли обслуживать (без аутентификации) |

Собственных расширений в теле запроса нет. Всё, что вы знаете про OpenAI API,
работает здесь.

---

## 4. Алиасы: какой когда брать

| Алиас | Когда брать | Не брать, если |
|---|---|---|
| `cheap-fast` | Классификация, извлечение полей, роутинг, простые преобразования. Основная рабочая лошадка для объёмных однотипных задач | Нужны длинные рассуждения или высокое качество текста |
| `balanced` | Дефолт. Если не знаете, что выбрать — берите его | Задача заведомо тривиальная (дешевле `cheap-fast`) |
| `smart` | Сложные задачи, длинные инструкции, качество важнее цены | Объёмный поток запросов: дорого |
| `reasoning` | Многошаговые рассуждения, где важен ход мысли | Нужна отказоустойчивость: **у алиаса нет fallback** |
| `long-context` | Вход в сотни тысяч токенов | Вход обычного размера — `balanced` дешевле |
| `vision` | Картинки на вход. **Единственный алиас с гарантией по картинкам** | Картинок нет |
| `embed-fast` | Эмбеддинги, когда важны цена и скорость | Нужно максимальное качество поиска |
| `embed-quality` | Эмбеддинги, когда важно качество | Большой объём: дороже |

Список **закрытый**. Вендорские имена моделей (`claude-opus-5`, `gpt-5`, …) в
поле `model` **запрещены** и возвращают `400`. Это не соглашение, а механизм:
таких целей маршрутизации в шлюзе физически нет.

### Что поддерживается

| Возможность | Где гарантирована |
|---|---|
| Streaming (SSE) | На всех chat-алиасах |
| Tools / function calling | На всех chat-алиасах |
| Structured output (`response_format` с `json_schema`) | На всех chat-алиасах, но со **статусом** — см. `GET /v1/models` |
| Vision (`image_url`, base64) | Только на `vision` |
| Embeddings | Только на `embed-*` |

**Про vision.** Шлюз не блокирует картинку на не-vision алиасе искусственно, и
часть моделей их поймёт. Но гарантия даётся только по `vision`. Если вашему
коду нужны картинки — используйте `vision`, иначе поведение может измениться
при следующем ремапе алиаса.

### `GET /v1/models` — актуальная матрица возможностей

Возвращает **только те алиасы, которые разрешены вашему ключу**. Проверять
поддержку не нужно спрашивая владельца — спросите шлюз:

```bash
curl -s https://<домен>/v1/models -H "Authorization: Bearer $GW_KEY" | jq
```

```json
{
  "object": "list",
  "data": [
    {
      "id": "cheap-fast",
      "object": "model",
      "owned_by": "llm-gateway",
      "capabilities": {
        "streaming": true,
        "tools": true,
        "json_schema": "emulated",
        "vision": false,
        "embeddings": false
      },
      "context_window": 200000,
      "max_output_tokens": 64000
    }
  ]
}
```

`json_schema` — трёхзначное поле:

* `native` — провайдер гарантирует валидность структуры;
* `emulated` — структура задаётся промптом, **гарантий нет**, валидируйте ответ
  у себя;
* `unsupported` — не поддерживается.

---

## 5. Ответ

Стандартный OpenAI-формат плюс два обязательства:

**`usage` присутствует всегда**, в том числе в streaming: шлюз принудительно
выставляет `stream_options.include_usage = true`, даже если вы этого не
сделали. Токены приходят в финальном чанке.

**Заголовки ответа:**

| Заголовок | Значение |
|---|---|
| `x-gw-model` | Реальная модель, которая отработала запрос |
| `x-gw-alias` | Запрошенный алиас |
| `x-gw-fallback` | `true`, если сработал fallback-провайдер |
| `x-gw-request-id` | Идентификатор для корреляции с логами шлюза |

Если вы пишете тикет владельцу — приложите `x-gw-request-id`. По телу запроса
найти ничего не получится: тела не логируются.

---

## 6. Ошибки

| Код | Когда | Что делать |
|---|---|---|
| `400` | Неизвестный алиас, вендорское имя модели, невалидный запрос | Починить запрос. Повтор не поможет |
| `401` | Ключ невалиден, отозван или истёк | Запросить новый ключ у владельца |
| `403` | Ключ валиден, но алиас ему не разрешён | Взять алиас из `available` или запросить расширение доступа |
| `429` | Исчерпан бюджет или rate-limit | См. ниже |
| `502` | Все провайдеры под алиасом вернули ошибку | Повторить с backoff; если стабильно — тикет владельцу |
| `504` | Превышен таймаут 30 s | Повторить; для длинных ответов использовать streaming |

Формат тела:

```json
{
  "error": {
    "code": "unknown_model_alias",
    "message": "Model 'claude-opus-5' is not a valid alias. Use one of the available aliases.",
    "type": "invalid_request_error"
  },
  "available": ["cheap-fast", "balanced"]
}
```

### Что делать при `429`

Тело `429` самодостаточно — ходить в документацию не нужно:

```json
{
  "error": {
    "code": "budget_exceeded",
    "message": "Daily budget for consumer 'tansultant-reactivation' exhausted.",
    "type": "rate_limit_error"
  },
  "retry_after": 18240,
  "budget": {
    "limit_usd": 5.0,
    "spent_usd": 5.02,
    "period": "daily",
    "resets_at": "2026-08-13T00:00:00Z"
  }
}
```

* `error.code = rate_limit_exceeded` — вы упёрлись в rpm/tpm. Повторите через
  `retry_after` секунд, снизьте параллелизм.
* `error.code = budget_exceeded` — исчерпан бюджет периода. Повторять раньше
  `budget.resets_at` бессмысленно: до сброса окна будет тот же ответ.
  Если это штатная нагрузка, а не авария — попросите владельца поднять бюджет
  в `config/consumers.yaml`.

### Ретраи и таймауты

Ретраи между провайдерами и fallback — **зона ответственности шлюза**, наружу
они не протекают. Вы видите либо успех, либо финальную ошибку, и **никогда не
ждёте дольше 30 s**.

Отсюда практическое следствие: не делайте агрессивных собственных ретраев на
`400`/`403` — они детерминированы. Ретраить имеет смысл `429` (по
`retry_after`), `502` и `504`.

---

## 7. Примеры

### curl

```bash
curl https://<домен>/v1/chat/completions \
  -H "Authorization: Bearer $GW_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "balanced",
    "messages": [{"role": "user", "content": "Суммируй в одном предложении: ..."}],
    "max_tokens": 256
  }'
```

### Python (openai SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://<домен>/v1",
    api_key=os.environ["GW_KEY"],
)

response = client.chat.completions.create(
    model="balanced",                      # алиас, не имя модели
    messages=[{"role": "user", "content": "Суммируй в одном предложении: ..."}],
    max_tokens=256,
)
print(response.choices[0].message.content)
print(response.usage.total_tokens)
```

Streaming с подсчётом токенов:

```python
stream = client.chat.completions.create(
    model="balanced",
    messages=[{"role": "user", "content": "Расскажи про облака"}],
    stream=True,
)
for chunk in stream:
    if chunk.choices and chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
    if chunk.usage:                        # придёт в финальном чанке всегда
        print(f"\ntokens: {chunk.usage.total_tokens}")
```

Tools:

```python
response = client.chat.completions.create(
    model="balanced",
    messages=[{"role": "user", "content": "Какая погода в Берлине?"}],
    tools=[{
        "type": "function",
        "function": {
            "name": "get_weather",
            "description": "Текущая погода в городе",
            "parameters": {
                "type": "object",
                "properties": {"city": {"type": "string"}},
                "required": ["city"],
            },
        },
    }],
    tool_choice="auto",
)
for call in response.choices[0].message.tool_calls or []:
    print(call.function.name, call.function.arguments)
```

Эмбеддинги:

```python
vectors = client.embeddings.create(model="embed-fast", input=["текст один", "текст два"])
print(len(vectors.data[0].embedding))
```

### Node.js

```js
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "https://<домен>/v1",
  apiKey: process.env.GW_KEY,
});

const response = await client.chat.completions.create({
  model: "cheap-fast",
  messages: [{ role: "user", content: "Классифицируй: положительный или отрицательный отзыв?" }],
  max_tokens: 16,
});

console.log(response.choices[0].message.content);
console.log(response.usage.total_tokens);
```

### n8n

Нода **HTTP Request**:

* Method: `POST`
* URL: `https://<домен>/v1/chat/completions`
* Authentication: `Generic Credential Type` → `Header Auth`
  * Name: `Authorization`
  * Value: `Bearer sk-gw-…`
* Send Body: `JSON`

```json
{
  "model": "cheap-fast",
  "messages": [
    {"role": "user", "content": "={{ $json.text }}"}
  ],
  "max_tokens": 256
}
```

Штатная нода **OpenAI** тоже работает: в credentials укажите Base URL
`https://<домен>/v1` и ключ `sk-gw-…`, а в поле модели впишите алиас вручную —
выпадающий список ноды заполняется из `GET /v1/models` и покажет только ваши
алиасы.

---

## 8. Что вам гарантируется при изменениях

Версионирование — SemVer.

* **MAJOR** — несовместимое изменение этого документа: переименование или
  удаление алиаса, изменение формата ошибки, смена схемы аутентификации.
  О таком предупреждают заранее.
* **MINOR** — новый алиас. Ваш код не ломается.
* **PATCH** — под алиасом сменилась вендорская модель. **Вы этого не заметите** —
  ровно ради этого шлюз и строился.

Практическое следствие: не зашивайте в код предположения о том, какая модель
стоит за алиасом, и не парсите `x-gw-model` в логике. Используйте
`GET /v1/models`, если нужно узнать возможности.
