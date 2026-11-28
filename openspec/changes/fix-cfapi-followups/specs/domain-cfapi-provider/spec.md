# domain-cfapi-provider Delta

## MODIFIED Requirements

### Requirement: Плейсхолдеры из метаданных hub

Система SHALL заселять sync root плейсхолдерами из метаданных hub полным рекурсивным обходом (walk) в два такта: верхний уровень — синхронно до маркера готовности, остальные уровни — фоновым walk, который запускается сразу после маркера и его не блокирует.

Walk SHALL обходить каждый каталог hub ровно один раз за запуск, начиная с каталогов верхнего уровня, и SHALL создавать плейсхолдеры записей каталога только после существования плейсхолдера самого каталога.

Каждая запись hub SHALL создаваться поштучно create-примитивом: реальная запись (новый пустой файл или новый каталог) конвертируется в плейсхолдер через `CfConvertToPlaceholder` с идентификатором записи hub в качестве FileIdentity. Система SHALL NOT записывать данные файла при создании и SHALL NOT перезаписывать существующую запись с тем же именем.

Файл-плейсхолдер SHALL иметь логический размер и время модификации из hub и SHALL оставаться дегидратированным: чтение его данных приводит к колбэку FETCH_DATA.

Ошибка create-примитива на одной записи SHALL влиять только на эту запись: созданная для неё реальная запись удаляется, ошибка фиксируется, остальные записи каталога создаются.

Walk SHALL быть идемпотентным: запись hub, имя которой уже существует в каталоге локально, повторно не создаётся. Классификация занятого имени SHALL быть идентичност-первой: запись, известная индексу по inode, SHALL пропускаться без предупреждения и без дублирования в журнале; запись, не известная индексу, SHALL классифицироваться по STANDARD-информации локальной записи (`CfGetPlaceholderInfo`): если FileIdentity локальной записи равен inode записи hub, запись SHALL усыновляться — индекс пополняется, запись учитывается как пропущенная, локальная запись SHALL NOT создаваться заново и SHALL NOT перезаписываться; если локальная запись не является плейсхолдером, несёт чужой FileIdentity или STANDARD-запрос не дал определённого ответа, запись SHALL считаться конфликтом: пропускается с одним предупреждением на каталог, walk продолжается, локальная запись не изменяется. Атрибутное состояние записи (RECALL_ON_DATA_ACCESS и связанные флаги) SHALL NOT быть решающим признаком классификации. Путь FETCH-наполнения каталога SHALL использовать ту же классификацию. При известном inode SHALL NOT выполняться STANDARD-запрос идентичности.

Транзиентная ошибка листинга каталога SHALL обрабатываться повтором с задержкой от 1с до 30с; перманентная — пропуском поддерева с предупреждением и продолжением walk. Строка `walk complete:` SHALL выводиться ровно один раз: когда очередь пуста и нет каталогов, ожидающих повтора.

Во время работы walk система SHALL выводить строку прогресса с префиксом `walk:` и счётчиками `dirs`, `files`, `skipped`, `conflicts`, `failed`, `pending`, `elapsed` не чаще одного раза в 5 секунд. Усыновлённые записи учитываются в `skipped`; при ненулевом количестве усыновлений система SHALL выводить отдельную строку `walk adopt: entries=<N>`.

#### Scenario: Верхний уровень
- **WHEN** `serve` запускается при доступном hub
- **THEN** записи верхнего уровня hub создаются плейсхолдерами синхронно, до вывода маркера готовности
- **AND** сразу после маркера запускается фоновый walk, начиная с каталогов верхнего уровня

#### Scenario: Дочерний каталог
- **WHEN** фоновый walk обрабатывает дочерний каталог hub
- **THEN** каждая его запись создаётся отдельным create-примитивом: реальная запись и конверсия в плейсхолдер
- **AND** подкаталоги ставятся в очередь walk после создания записей каталога
- **AND** записи, чьи имена уже существуют локально, не создаются повторно

#### Scenario: Перезапуск без дублей
- **WHEN** `serve` запускается повторно на уже заселённом sync root при неизменном hub и сохранённом state-dir
- **THEN** walk пропускает все существующие плейсхолдеры без предупреждений
- **AND** в журнал не добавляется ни одной записи
- **AND** записи, не созданные из-за прерывания предыдущего запуска, создаются

#### Scenario: Состояние созданного файла
- **WHEN** walk создаёт файл-плейсхолдер для записи hub размером N
- **THEN** логический размер плейсхолдера равен N, время модификации равно времени модификации записи hub
- **AND** чтение данных плейсхолдера приводит к колбэку FETCH_DATA

#### Scenario: Отказ конверсии записи
- **WHEN** create-примитив завершается ошибкой на одной записи каталога
- **THEN** созданная для неё реальная запись удаляется, в индекс она не попадает
- **AND** ошибка фиксируется счётчиком `failed` и предупреждением на каталог
- **AND** остальные записи каталога создаются

#### Scenario: Конфликт имён
- **WHEN** в обрабатываемом каталоге локально существует запись, которая не является плейсхолдером, несёт чужой FileIdentity или чей STANDARD-запрос не дал определённого ответа, и в hub есть запись с тем же именем
- **THEN** плейсхолдер для записи hub не создаётся
- **AND** выводится одно предупреждение на каталог с количеством и примерами имён
- **AND** локальная запись не изменяется

#### Scenario: Усыновление синхронизированного плейсхолдера
- **WHEN** рестарт-walk встречает локальную запись, чьи спец-атрибуты очищены фильтром (CF_PLACEHOLDER_STATE=0), но STANDARD-запрос подтверждает плейсхолдер с FileIdentity, равным inode записи hub, и запись отсутствует в индексе
- **THEN** запись усыновляется: индекс пополняется, запись учитывается как пропущенная
- **AND** предупреждение о конфликте не выводится, локальная запись не изменяется
- **AND** выводится строка `walk adopt: entries=<N>`

#### Scenario: Ошибка STANDARD-запроса
- **WHEN** STANDARD-запрос идентичности локальной записи завершается ошибкой
- **THEN** запись считается конфликтом без создания, усыновления и дегидратации
- **AND** следующая классификация этой записи повторяется на последующем walk

#### Scenario: Сбой листинга в глубине
- **WHEN** листинг каталога во время фонового walk завершается транзиентной ошибкой
- **THEN** каталог повторяется с задержкой от 1с до 30с
- **AND** строка `walk complete:` не выводится, пока каталог не обработан
- **AND** доступ к заселённым каталогам и гидрация не прерываются

#### Scenario: Каталог, не достигнутый walk
- **WHEN** пользователь перечисляет каталог-плейсхолдер, который walk ещё не обработал
- **THEN** провайдер не получает колбэк FETCH_PLACEHOLDERS
- **AND** после обработки каталога walk его записи присутствуют в sync root

#### Scenario: Прерывание walk
- **WHEN** `serve` останавливается во время фонового walk
- **THEN** выводится строка `walk cancelled:`, а строка `walk complete:` не выводится
- **AND** при следующем запуске walk создаёт только отсутствующие записи

Verified-by: pkg/cfapi/identity_test.go::TestFileIdentityBytes
Verified-by: pkg/cfapi/identity_test.go::TestIsNotAPlaceholder
Verified-by: pkg/cfprovider/identity_test.go::TestClassifyIdentity
Verified-by: pkg/cfprovider/identity_test.go::TestClassifyOccupiedIdentityFirst
Verified-by: pkg/cfprovider/identity_test.go::TestClassifyOccupiedKnownNoStandardQuery
Verified-by: pkg/cfprovider/identity_test.go::TestClassifyOccupiedAbsentCreates
Verified-by: pkg/cfprovider/identity_test.go::TestBuildPopulatedDir
Verified-by: pkg/cfprovider/walk_test.go::TestWalkAdoptionNormalizesHydratedPlaceholder
Verified-by: pkg/cfprovider/walk_test.go::TestWalkAdoptionDehydratedSkipsNormalization
Verified-by: pkg/cfprovider/walk_test.go::TestWalkExistsReclassifyRestore

### Requirement: Сверка при старте

При запуске `serve` SHALL ставить в очередь выгрузки локальные файлы без плейсхолдера и плейсхолдеры вне состояния in-sync, а в очередь удаления — записи индекса, отсутствующие на диске в каталогах с завершённым наполнением.

Дополнительно walk SHALL сверять каждую свою запись-файл (известную индексу или усыновлённую) с записью hub по размеру (строго) и времени модификации (с допуском 2 секунды): при совпадении — без мутаций и строк журнала; при более новом локальном файле — постановка в очередь выгрузки существующим механизмом (при `--read-only` — только предупреждение; при out-of-sync по фильтру — ветка остаётся за очередью выгрузки сверки); при более новой записи hub — ревалидация плейсхолдера: обновление size/mtime с дегидратацией (`CfUpdatePlaceholder`, `UpdateFlagVerifyInSync|UpdateFlagDehydrate`) после супрессии собственного watcher-события; при pinned или out-of-sync локальной записи — пропуск с предупреждением; при разошедшемся размере и времени в пределах допуска — пропуск с предупреждением без мутаций. Каталоги SHALL быть исключены из сверки свежести. Сверка SHALL NOT писать в индекс и SHALL быть идемпотентной: рестарт без изменений выводит ноль строк свежести. Строка `walk fresh: revalidated=<N> queued-upload=<N> readonly=<N> pinned=<N> ambiguous=<N> out-of-sync=<N>` SHALL выводиться при ненулевой сумме счётчиков.

#### Scenario: Восстановление после сбоя
- **WHEN** `serve` запускается после сбоя, в sync root есть изменённый плейсхолдер вне in-sync и удалённый плейсхолдер в наполненном каталоге
- **THEN** первый выгружается, второй удаляется в backend

#### Scenario: Правка с другой машины подтягивается на рестарте
- **WHEN** файл-плейсхолдер изменён клиентом с другой машины при выключенном `serve` (размер или время записи hub новее локального), затем `serve` перезапускается
- **THEN** walk ревалидирует плейсхолдер: size/mtime обновлены, локальный контент дегидратирован
- **AND** первое чтение файла возвращает новую версию содержимого (FETCH_DATA)

#### Scenario: Локальная правка при выключенном serve выгружается
- **WHEN** гидратированный файл-плейсхолдер изменён локально при выключенном `serve` (локальный mtime новее записи hub), затем `serve` перезапускается
- **THEN** walk ставит файл в очередь выгрузки
- **AND** после выгрузки содержимое и время доступны другим клиентам

#### Scenario: Рестарт без изменений
- **WHEN** `serve` перезапускается на неизменном корне
- **THEN** строка `walk fresh:` не выводится, ни один плейсхолдер не обновляется и не ставится в очередь

#### Scenario: Pinned устарел
- **WHEN** ревалидация встречает pinned-плейсхолдер с более новой записью hub
- **THEN** пропуск с предупреждением, без мутаций

#### Scenario: Двусторонняя правка
- **WHEN** размер локального файла и записи hub разошлись, а время модификации в пределах допуска
- **THEN** пропуск с предупреждением, без мутаций

Verified-by: pkg/cfprovider/freshness_test.go::TestCompareVersion
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessEqualNoMutations
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessStatError
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessDirSkipped
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessPathMismatchSkipped
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessLocalNewerQueuesUpload
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessLocalNewerReadOnly
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessLocalNewerOutOfSyncLeftToReconcile
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessBackendNewerRevalidates
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessBackendNewerPinnedSkips
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessBackendNewerOutOfSyncSkips
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessAmbiguousSkipsNoMutations
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessStateUnavailableSkips
Verified-by: pkg/cfprovider/freshness_test.go::TestCheckFreshnessAdoptDivergedSkipsNormalization
Verified-by: pkg/cfprovider/walk_test.go::TestWalkIdempotentSecondRun

## ADDED Requirements

### Requirement: Cleanup-режим осиротевших плейсхолдеров

Команда `juicefs clouddrive cleanup --root <корень>` SHALL анализировать каталог корня без подключённого провайдера и без мутаций (dry-run по умолчанию): классифицировать каждую запись по STANDARD-информации (плейсхолдер / не-плейсхолдер / неизвестно / ссылка) и выводить отчёт с счётчиками и планом действий. При работающем провайдере на корне (или вложенном живом sync root) команда SHALL отказывать без мутаций.

#### Scenario: Dry-run без мутаций
- **WHEN** выполняется `cleanup --root X` без флагов на корне с осиротевшими плейсхолдерами
- **THEN** выводится отчёт классификации и план, файловая система, реестр и state-dir не изменяются

#### Scenario: Провайдер работает
- **WHEN** выполняется `cleanup --root X` при живом `serve` на этом корне
- **THEN** команда завершается отказом до первой мутации

Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupClassifyAndDryRun
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupByAttributesFallback
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupDryRunNestedLiveRootSkips
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupApplyNestedLiveRootRefuses
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupRefuseMutexHeld
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupRefusePreconditions
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupStateDirUntouched

### Requirement: Уровни применения cleanup

С мутационными флагами SHALL применяться уровни: `--apply` — revert плейсхолдеров в обычные записи через `CfRevertPlaceholder` с пост-проверкой; `--apply --delete-files` — дополнительно удаление подтверждённо-reverted записей, а при отказе revert'а (живой факт U-C1: revert — provider-mediated, на мёртвом провайдере HRESULT 0x80070194) — прямое удаление доказанно дегидратированных сирот (контент в backend, fallback `removed-direct`; каталог — только пустой) с пост-проверкой отсутствия; гидратированная сирота при отказе revert'а SHALL давать ошибку с rescue-указанием (возможен локальный контент, отсутствующий в backend) и SHALL NOT удаляться; запись, классифицированная только по атрибутам (STANDARD не дал доказательства), SHALL NOT удаляться через fallback; `--apply --delete-files --unregister` — дополнительно снятие регистрации sync root и очистка ключа реестра, выполняемое ПОСЛЕДНИМ шагом после revert/удалений (CfUnregisterSyncRoot удаляет дегидратированные скелеты и материализует гидратированные). Команда SHALL NOT удалять записи, не бывшие плейсхолдерами; SHALL NOT использовать рекурсивное удаление каталогов; каталоги SHALL удаляться только пустыми. Применение к реальным корням SHALL выполняться владельцем отдельно (в настоящем change — только dry-run проверка).

#### Scenario: Revert и удаление
- **WHEN** выполняется `cleanup --root X --apply --delete-files` на dry-run-плане с плейсхолдерами
- **THEN** каждый плейсхолдер revertnut и после подтверждения удалён
- **AND** обычные файлы и каталоги с обычными файлами внутри сохранены

#### Scenario: Сирота с мёртвым провайдером
- **WHEN** revert дегидратированного плейсхолдера падает (провайдер не отвечает) и передан `--delete-files`
- **THEN** запись удаляется напрямую с пост-проверкой отсутствия и учитывается как `removed-direct`
- **AND** гидратированная сирота в том же прогоне даёт ошибку с rescue-указанием и остаётся на месте

#### Scenario: Идемпотентность
- **WHEN** cleanup выполняется повторно после успешного применения
- **THEN** dry-run показывает ноль плейсхолдеров

Verified-by: pkg/cfapi/operations_test.go::TestRevertPlaceholderByPathPlainFile
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupApplyRevertNoDelete
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupApplyDeleteFiles
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupApplyDirRevertBlocked
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupPostCheckKeepsFile
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupRegisteredRequiresUnregister
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupUnregisterAfterRevert
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupDehydratedFallbackDirectRemove
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupHydratedRevertFailureKeepsError
Verified-by: pkg/cfprovider/cleanup_test.go::TestCleanupIdempotentSecondRun
