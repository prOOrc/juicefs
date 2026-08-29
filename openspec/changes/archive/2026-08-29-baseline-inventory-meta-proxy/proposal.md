# Proposal: baseline-inventory-meta-proxy

## What & Why

Инвентаризация (as-is) капабилити **domain-meta-proxy** — gRPC-прокси JuiceFS metadata для внешних клиентов AGIO Drive. Код реализован на ветке `agio-drive-v2` (сервер `juicefs meta-proxy`, клиент `grpcMeta`, OIDC authn, authz через agio-platform), но не описан в Source of Truth.

Цель — зафиксировать фактическое поведение: CLI и флаги, поверхность gRPC-сервиса `MetaService` (78 RPC), OIDC-аутентификация (strict interceptors), авторизация (unary-интерцептор с маппингом RPC→право, кэш решений, InodePathCache, root short-circuit, фильтрация листингов), stateful DirHandler, gRPC-клиент `grpcMeta` (кэши, heartbeat, singleflight). Это база для per-stage changes шифрования (SRS-001) и будущих изменений authn/authz.

Методология: код — источник правды; требования ретроактивно извлечены из реализации (as-is inventory). Отклонения, не имеющие смысла как контракт, формулируются как intended contract и фиксируются в design.md → Known Deviations.

## Capabilities

- `domain-meta-proxy` — НОВЫЙ capability: gRPC Meta Proxy с OIDC authn и authz через agio-platform.

## Non-goals

- Не описывать upstream JuiceFS (meta-движки, vfs, chunk) — только AGIO-специфичный прокси.
- Не менять поведение кода; это инвентаризация, не фича.
- Не покрывать шифрование (SRS-001) — реализуется per-stage changes поверх этого инвентаря.
- Не описывать outbox (отдельный capability `domain-outbox`, инвентаризирован на ветке `outbox`).
- Не описывать agio-platform AuthzService как капабилити этого репозитория — только контракт gRPC (`agio.platform.authz.v1.AuthzService`), который прокси потребляет.

## Related Requirements

- SRS-001 (review, Final draft v2.2): Подсистема шифрования agio Drive — `specs/srs/SRS-001-agio-drive-encryption.md`. Meta Proxy — точка гейтинга выдачи FEK (§6, §7); инвариант идентичности OIDC `sub` ≡ `user.id` (§1.5 S12) подтверждён as-is: authz-интерцептор берёт userID из `idToken.Subject` без маппинга.
- Архитектурный план v13 (рабочий документ): `.qwen/plans/agio-drive-full-plan-v13.md` — целевая архитектура (Meta Proxy как единственная точка контроля доступа).
- BRD для agio Drive отсутствует — инвентаризация создаёт первый формальный слой требований по капабилити.
