# domain-meta-proxy Delta

## ADDED Requirements

### Requirement: Credential masking in command logs

Команда `juicefs meta-proxy` SHALL NOT выводить в логи пароль metadata-движка: любое лог-сообщение, содержащее `--meta-backend` URL, SHALL печатать URL с замаскированным паролем (формат `redis://:****@host:port/db`) — тем же способом, что и лог metadata-адреса в `meta.NewClient`. Маскировка SHALL применяться безусловно, на всех уровнях логирования.

#### Scenario: Startup log masks the backend password

- **WHEN** прокси запускается с `--meta-backend` URL, содержащим пароль
- **THEN** строка лога с metadata backend URL содержит `:****@` и не содержит подстроку пароля

#### Scenario: No unmasked backend URL log statements remain

- **WHEN** выполняется поиск лог-вызовов с `metaBackendUrl` (или иным meta URL) без маскировки в `cmd/meta_proxy.go`
- **THEN** такие вызовы отсутствуют — каждый вывод URL проходит через `utils.RemovePassword`
