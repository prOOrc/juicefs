## Purpose

Запуск `juicefs gateway` в platform-режиме поверх grpc meta-proxy: обязательные входы (env), стартовые проверки fail-fast, гигиена секретов. Обычный (redis/minio-IAM) режим gateway не меняется (ADR-005 agio-platform).

## ADDED Requirements

### Requirement: Platform mode startup inputs

Gateway SHALL поддерживать platform-режим, включаемый при запуске с grpc meta-URL и набором обязательных env: service-token (к прокси), адрес keymanager платформы, node-credential gateway (к платформе), адрес authz-сервиса + имя тома (для authz-резолва путей), facility id, имя бакета. Отсутствие любого обязательного входа SHALL вызывать ошибку старта с именованием недостающего ключа; S3-порта не поднимается. Значения секретных env SHALL NOT попадать в логи (лог печатает только имена ключей конфигурации).

#### Scenario: Missing mandatory input fails fast

- **WHEN** gateway запущен в platform-режиме без service-token (или без любого другого обязательного env)
- **THEN** процесс завершается ошибкой старта с именованием недостающего ключа, S3-порта не открывается

#### Scenario: Secrets are not logged

- **WHEN** gateway стартует в platform-режиме (успешно или с ошибкой)
- **THEN** в логах отсутствуют значения service-token и node-credential; присутствуют только имена ключей

### Requirement: Platform mode forbidden legacy surfaces

В platform-режиме: `--subdir` SHALL вызывать ошибку старта; `--refresh-iam-interval` SHALL вызывать ошибку старта; выставленные в окружении `MINIO_ROOT_USER` или `MINIO_ROOT_PASSWORD` SHALL вызывать ошибку старта (root-ключа в режиме не существует, его наличие — конфигурационная ошибка, а не игнорируемый фактор).

#### Scenario: subdir is refused

- **WHEN** gateway в platform-режиме запущен с `--subdir companies/acme`
- **THEN** процесс завершается ошибкой старта (path-контракт authz требует полных путей от корня тома)

#### Scenario: refresh-iam-interval is refused

- **WHEN** gateway в platform-режиме запущен с `--refresh-iam-interval`
- **THEN** процесс завершается ошибкой старта (IAM-подсистема в режиме подавлена)

#### Scenario: Root credentials are refused

- **WHEN** в окружении процесса выставлены MINIO_ROOT_USER/MINIO_ROOT_PASSWORD и gateway запускается в platform-режиме
- **THEN** процесс завершается ошибкой старта, не поднимая S3-порту

### Requirement: Platform mode service connectivity check

Перед поднятием S3-порты gateway SHALL установить grpc-соединение к meta-proxy и выполнить GetFormat от service-identity; отказ соединения или отказ проверки (невалидный service-token, чужой том) SHALL завершать запуск с ошибкой. Успешная проверка фиксирует, что прокси принял service-соединение и том доступен.

#### Scenario: Wrong volume token rejected at startup

- **WHEN** gateway запущен с service-token, который прокси отклонил (токен другого тома/сервиса)
- **THEN** процесс завершается ошибкой старта; S3-клиенты не подключаются

#### Scenario: Successful startup implies service connectivity

- **WHEN** gateway стартовал успешно в platform-режиме
- **THEN** GetFormat от service-identity был выполнен успешно до открытия S3-порты

### Requirement: Multipart complete writes final object at destination

`CompleteMultipartUpload` в jfsObjects (`pkg/gateway/gateway.go`) SHALL создавать финальный объект непосредственно по целевому пути тома (новый inode; на шифрованном томе CreateFileKey выполняется по реальному пути компании), а не создавать complete-файл в служебном каталоге `.sys/uploads/` с последующим rename. После успешного Complete staging-каталог uploadID SHALL удаляться; при ошибке копирования частей частично записанный целевой файл SHALL удаляться с записью в лог. `AbortMultipartUpload` и фоновая очистка `.sys/tmp/`/`.sys/uploads/`/trash SHALL остаться без изменений.

#### Scenario: Complete keys the final object by its real path

- **WHEN** multipart-загрузка завершается для объекта `companies/acme/render.exr` на шифрованном томе
- **THEN** FEK финального файла создан по пути `companies/acme/render.exr` (реальная компания); чтение объекта через GetFileFEK по этому пути успешно

#### Scenario: Failed complete leaves no partial object

- **WHEN** копирование частей в целевой файл завершилось ошибкой
- **THEN** частичный файл по целевому пути удалён, в логе ошибка, staging очищается фоновой очисткой

#### Scenario: Unencrypted volumes unaffected

- **WHEN** Complete выполняется на нешифрованном томе
- **THEN** поведение семантически идентично прежнему (новый файл по целевому пути, staging удалён)
