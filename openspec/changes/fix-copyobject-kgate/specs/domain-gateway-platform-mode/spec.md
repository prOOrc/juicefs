## ADDED Requirements

### Requirement: Server-side copy writes final object at destination

Шлюз SHALL создавать файл объекта-получателя server-side CopyObject (источник и
получатель различаются) по финальному пути этого объекта и SHALL NOT создавать для
этого копирования промежуточные файлы во временном каталоге `.sys/tmp` бакета-получателя.
Перезапись существующего файла-получателя SHALL повторять S3-семантику молчаливой
перезаписи; занятие пути получателя каталогом SHALL завершаться ошибкой; перенос
etag/tags/метаданных источника SHALL выполняться по действующим директивам запроса
(логика `KeepEtag`/`ObjTag`/`setObjMeta` не меняется). Same-key сериализация записей
SHALL сохраняться (`writeLocks` на уровне `CopyObject`).

#### Scenario: Копирование в несуществующий объект
- **WHEN** вызван CopyObject с различающимися источником и получателем, получатель
  не существует, бакет-получатель существует
- **THEN** файл получателя создан по пути объекта-получателя, содержимое и ETag
  получателя равны содержимому и ETag источника, во временном каталоге бакета-получателя
  не появилось новых файлов
- **Verified-by:** pkg/gateway/gateway_test.go::TestCopyObjectAtDestination

#### Scenario: Родительские каталоги получателя отсутствуют
- **WHEN** бакет-получатель существует и родительские каталоги пути получателя
  не существуют
- **THEN** копирование завершено без ошибки и содержимое получателя равно
  содержимому источника
- **Verified-by:** pkg/gateway/gateway_test.go::TestCopyObjectAtDestination

#### Scenario: Перезапись существующего файла
- **WHEN** по пути получателя существует обычный файл с иным содержимым
- **THEN** после CopyObject по пути получателя находится файл с содержимым источника,
  его ETag и метаданные получены от источника согласно директивам запроса
- **Verified-by:** pkg/gateway/gateway_test.go::TestCopyObjectAtDestinationReplacesExisting

#### Scenario: Путь получателя занят каталогом
- **WHEN** по пути получателя существует каталог
- **THEN** CopyObject возвращает ошибку, содержимое каталога и объект-источник
  не изменены
- **Verified-by:** pkg/gateway/gateway_test.go::TestCopyObjectAtDestinationDestinationIsDirectory

#### Scenario: Источник не изменяется
- **WHEN** CopyObject завершён без ошибки
- **THEN** содержимое, ETag и метаданные объекта-источника равны значениям до
  копирования, повторное копирование из него проходит
- **Verified-by:** pkg/gateway/gateway_test.go::TestCopyObjectAtDestinationKeepsSource

#### Scenario: Same-key запись сериализуется
- **WHEN** во время in-flight PutObject в ключ выполняется CopyObject в тот же ключ
- **THEN** CopyObject ожидает завершения PutObject (без изменения
  TestObjectWriteSerialization)
- **Verified-by:** pkg/gateway/gateway_test.go::TestObjectWriteSerialization

### Requirement: Server-side copy failure leaves no destination file

Шлюз SHALL удалять файл объекта-получателя server-side CopyObject, если после его
создания любой последующий фатальный шаг копирования (перенос содержимого, `Stat`)
завершился ошибкой. Ошибки открытия несуществующего источника SHALL обнаруживаться
до создания/перезаписи получателя. Отсутствующий бакет-получатель SHALL давать
`BucketNotFound` без создания файла и бакета.

#### Scenario: Ошибка после создания файла получателя
- **WHEN** после успешного создания файла получателя фатальный шаг (CopyFileRange
  или Stat) завершился ошибкой
- **THEN** CopyObject возвращает эту ошибку и по пути получателя не остаётся файла,
  включая подслучай перезаписи
- **Verified-by:** pkg/gateway/gateway_test.go::TestCopyObjectAtDestinationFailureRemovesPartial

#### Scenario: Источник отсутствует
- **WHEN** объект-источник не существует на момент вызова
- **THEN** CopyObject возвращает ошибку, существующий файл получателя остаётся
  без изменений
- **Verified-by:** pkg/gateway/gateway_test.go::TestCopyObjectAtDestinationSourceMissingKeepsDestination

#### Scenario: Бакет-получатель не существует
- **WHEN** бакет-получатель не существует
- **THEN** CopyObject возвращает `BucketNotFound`, файл получателя не создан,
  бакет-получатель не создан
- **Verified-by:** pkg/gateway/gateway_test.go::TestCopyObjectAtDestinationAfterBucketDeleted
