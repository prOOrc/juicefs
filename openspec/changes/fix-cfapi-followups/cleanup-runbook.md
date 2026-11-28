# Runbook: чистка осиротевших корней AgioDrive [OWNER-GATE]

Подготовлено change'ом `fix-cfapi-followups` 28.11.2026 по итогам живых проб на AgioDrive8.
**Применение к реальным корням — только по явному GO владельца.** Бинарь: сборка ветки
`feature/fix-cfapi-followups` (любая после 28.11; рецепты сборки — архивный change, PATH
C:\mingw64\bin + CGO_ENABLED=1 + CGO_CFLAGS='-IC:/WinFsp/inc/fuse').

## Живые факты проб (важно понимать перед применением)

1. **Незарегистрированный корень сирот не хранит**: `CfUnregisterSyncRoot` сам конвертирует
   плейсхолдеры — гидратированные материализуются в plain (контент сохраняется локально),
   дегидратированные скелеты УДАЛЯЕТ (контент остаётся только в backend). Корни, с которых
   снимали регистрацию, уже чисты.
2. **`CfRevertPlaceholder` — provider-mediated**: на корне с мёртвым провайдером падает
   HRESULT 0x80070194. Cleanup работает с мёртвым провайдером (поэтому revert внутри
   cleanup падает и включается fallback — прямое удаление).
3. **Прямое удаление (os.Remove/rm) работает на сиротах без провайдера** — и на
   дегидратированных, и на гидратированных (U-C4, exit 0). Для дегидратированных это
   безопасно (контент в backend); гидратированная сирота может нести ЛОКАЛЬНЫЙ контент,
   которого нет в backend — перед удалением такой записи её содержимое стоит сохранить.
4. **Pin CLI и status требуют явный `--root`** (дефолт — legacy `%USERPROFILE%\AgioDrive`).

## Запрещено

- **AgioDrive9** — живой facility-корень владельца (vector-stage).
- **AgioDrive7** — sqlite-плейграунд владельца (smoke/cycles).
- **AgioDrive** — plain-остаток, но внутри `file.txt.txt`/`companies` — сверить содержимое
  с владельцем перед `rd /s /q` (вердикт 27.11).

## Порядок для каждого корня (AgioDrive3, AgioDrive4, AgioDrive5, AgioDrive6, AgioDrive10)

```bat
:: 1. Убедиться, что на корне нет живого serve (иначе cleanup откажет — R2)
tasklist | findstr jfs-cfapi
::   если процесс обслуживает этот корень — сначала разобраться, что это за serve.

:: 2. Dry-run: отчёт без мутаций
juicefs-cfapi-verify3.exe clouddrive cleanup --root C:\Users\iobukhov\AgioDrive<N>

:: 3. Разобрать отчёт:
::    Placeholders=0, Plain=N  -> регистрация уже снята/плейсхолдеры сконвертированы;
::                                с файлами решает владелец (обычные удаление/архив)
::    Placeholders>0, все dehydrated -> контент в старых тестовых backend (jfs-tmp),
::                                можно чистить:
juicefs-cfapi-verify3.exe clouddrive cleanup --root C:\Users\iobukhov\AgioDrive<N> --apply --delete-files --unregister
::    Есть hydrated-сироты -> ошибки apply с rescue-подсказкой; содержимое таких файлов
::    сохранить вручную (через временный serve со «спасательным» meta, если известен
::    исходный том, либо скопировать после разборки с владельцем).

:: 4. Пустой каталог удалить вручную
rd /s /q C:\Users\iobukhov\AgioDrive<N>
```

## Состояние на 28.11 (после проб)

- AgioDrive8: зарегистрирован→clean→**незарегистрирован, каталог пуст** (пробный стенд,
  можно удалить каталог).
- AgioDrive11: тестовый корень «второй машины» — unregister/удаление в финальной уборке
  сессии.
- Общий ключ реестра `Explorer\SyncRootManager\AgioDrive!<SID>!Agio` один на все корни
  (providerId константен) — после unregister любого корня ключ удаляется целиком; рабочая
  регистрация восстанавливается `register --root <корень>`. Для приложения V3 это
  открытый вопрос мульти-корня (P-MULTIROOT, дизайн очереди B).
