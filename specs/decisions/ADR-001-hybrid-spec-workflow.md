---
id: ADR-001
type: adr
title: Гибридный spec-driven workflow (BRD/SRS + OpenSpec)
status: accepted
created: 2026-08-29
updated: 2026-08-29
---

# ADR: Гибридный spec-driven workflow (BRD/SRS + OpenSpec)

## Контекст

Форк развивается с активным использованием AI-агентов. Системные требования (SRS шифрования agio Drive, версии 0.13 → 2.2) и архитектурные планы (agio-drive-full-plan v1–v13, stage-планы 01–10) уже пишутся, но хранятся в gitignored-каталоге `.qwen/` — вне контроля версий и ревью. Для агентной реализации нужен машиночитаемый слой требований с чётким жизненным циклом изменений.

Практика гибридного workflow уже проверена в смежном проекте agio-platform (ADR-001 там, внедрён 2026-08-28): человеческий слой `specs/` + машинный слой `openspec/`.

Рассматривались два подхода:
1. **Чистый OpenSpec**: без слоя BRD/SRS, бизнес-контекст в `openspec/config.yaml` + ADR + capability specs.
2. **Гибридный**: человеческий слой `specs/` (BRD/SRS/ADR) + машинный слой `openspec/`.

## Варианты

### Вариант 1: Чистый OpenSpec

Плюсы:
- Меньше документов, один источник требований.

Минусы:
- Существующие SRS/планы (1000+ строк, итеративно доведённые до Final draft) пришлось бы переписывать в формат capability specs.
- Нет места для бизнес-документов для стейкхолдеров (BRD agio Drive будет писан).

### Вариант 2: Гибридный (BRD/SRS + OpenSpec)

Плюсы:
- Сохраняет существующие документы и их формат.
- Трассируемость: BRD → SRS → OpenSpec change → код.
- OpenSpec отвечает за машиночитаемое поведение (SHALL/WHEN/THEN) и жизненный цикл изменений (propose → apply → archive).
- Единая практика с agio-platform — обмен паттернами между проектами.

Минусы:
- Два слоя — нужно поддерживать согласованность (ссылки по ID в proposal.md).

## Решение

Выбран **Вариант 2 (гибридный)**:

- `specs/brd/`, `specs/srs/`, `specs/decisions/` — человеческий слой, коммитится в git.
- `openspec/specs/` — Source of Truth (capability specs), `openspec/changes/` — дельты.
- Связь слоёв: обязательный раздел "Related Requirements" в proposal.md со ссылками на ID из `specs/index.md`.
- Актуальная версия SRS шифрования (v2.2) перенесена из `.qwen/specs/` в `specs/srs/SRS-001-agio-drive-encryption.md`; исторические версии остаются в `.qwen/specs/`.
- Агентный tooling (`.qwen/commands/`, `.qwen/skills/`) коммитится через исключения в `.qwen/.gitignore` для консистентности команды.

Особенность относительно agio-platform: **upstream JuiceFS не спекается** — Source of Truth покрывает только AGIO-специфичные капабилити (Meta Proxy, outbox, шифрование и т.д.). Upstream-код меняется редко и описывается своей документацией.

## Последствия

- Каждый OpenSpec change обязан ссылаться на BRD/SRS/ADR (или объяснять их отсутствие).
- Изменение утверждённых SRS требует обновления документа и `specs/index.md`.
- Архитектор (`.qwen/agents/architect.md`) ведёт оба слоя; кодинг-агент работает только с `openspec/changes/`.
- Source of Truth (`openspec/specs/`) заполняется инвентаризационным change по AGIO-капабилити и далее поддерживается через archive.
- Шифрование (SRS-001) реализуется серией per-stage changes (по одному на stage-план 01–10).
