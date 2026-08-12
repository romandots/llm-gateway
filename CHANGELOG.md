# Changelog

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/),
версионирование — [SemVer](https://semver.org/lang/ru/).

* **MAJOR** — обратно несовместимое изменение публичного контракта
  (переименование или удаление алиаса, изменение формата ошибки, смена схемы
  аутентификации).
* **MINOR** — новый алиас, новая команда `gwctl`, новый провайдер.
* **PATCH** — багфикс, ремап алиаса на другую вендорскую модель (контракт не
  меняется).

## [Unreleased]

### Added

- **Публичный контракт** (`docs/CONTRACT.md`): OpenAI-совместимые
  `/v1/chat/completions`, `/v1/embeddings`, `/v1/models` и публичные
  `/health/liveness`, `/health/readiness`.
- **Таксономия из 8 алиасов**: `cheap-fast`, `balanced`, `smart`, `reasoning`,
  `long-context`, `vision`, `embed-fast`, `embed-quality`. Вендорские имена
  моделей на входе запрещены двумя независимыми барьерами: в `model_list`
  прокси существуют только алиасы, и у каждого ключа заполнен whitelist.
- **`gwctl`** — control plane на Go: `validate`, `diff`, `apply`,
  `key issue|list|revoke|rotate`, `spend`, `models`, `health`, `version`.
  Reconcile идемпотентен, секреты печатаются один раз и нигде не хранятся.
- **Декларативный конфиг** `config/models.yaml`, `config/consumers.yaml`,
  `config/proxy.yaml` со строгим парсингом: опечатка в имени поля и
  дублирующийся ключ — ошибки, а не «побеждает последний».
- **Fallback между провайдерами** по порядку `targets`: срабатывает на `429`,
  `5xx` и таймаут, не срабатывает на `400`.
- **Стек развёртывания**: docker compose с LiteLLM, Postgres, Redis и Caddy.
  Наружу опубликован только Caddy.
- **Контрактные хуки прокси** (`deploy/litellm/gw_hooks.py`): принудительный
  `stream_options.include_usage`, контрактные тела ошибок
  `unknown_model_alias`, `alias_not_permitted`, `budget_exceeded` с
  `retry_after` и остатком бюджета.
- **Makefile** как единая точка входа для сборки, стека, управления и
  обслуживания; цели, меняющие состояние, требуют подтверждения или `YES=1`.
- **Smoke-тест контракта** (`test/smoke/smoke.sh`): проверяет каждый заявленный
  флаг возможностей реальным запросом, включая отсутствие тел запросов в логах.
- **Документация**: `README.md`, `docs/CONTRACT.md`, `docs/GWCTL.md`,
  `docs/RUNBOOK.md`.

### Security

- Вендорские ключи не покидают VPS: в конфигурации хранится ссылка
  `os.environ/…`, а не значение.
- Тела запросов и ответов не логируются; в spend-логах только метаданные.
- `gwctl validate` проверяет, что в `config/` нет секретов и что `.env` закрыт
  `.gitignore`.
