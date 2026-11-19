# domain-outbox Specification (Delta)

## MODIFIED Requirements

### Requirement: Consumer group and polling

The consumer SHALL poll with `XREADGROUP` using consumer name `juicefs`, only-new-message id `>`,
batch count 100, block timeout 1 second, driven by a 1-second ticker; a consumer group SHALL be
initialized (creating the stream if absent) starting from the latest message (`$`); starting a
second consumer on the same outbox instance SHALL return an error.

Цикл консюмера SHALL дополнительно периодически выполнять `XAutoClaim` с `MinIdle` из флага
`--reclaim-min-idle` (default 60s) и передавать reclaimed-сообщения в общий путь обработки
(`processMessage`).

`StopConsumer` SHALL отменять контекст цикла (`cancel`), сбрасывать признак running и SHALL
возвращать управление только после фактического завершения горутины цикла (закрытие
done-канала при выходе из `consumeLoop`); ожидание завершения SHALL выполняться вне блокировки
`mu`. Повторный `StartConsumer` после `StopConsumer` SHALL запускать ровно один новый цикл
(предыдущий гарантированно завершён к моменту возврата `StopConsumer`).

#### Scenario: Group created at latest offset

- **WHEN** the consumer group does not exist yet
- **THEN** it SHALL be created with start id `$` so that pre-existing stream messages are not replayed

#### Scenario: Stale pending message is reclaimed

- **WHEN** сообщение находится в PEL дольше `--reclaim-min-idle` (например, консюмер упал между
  чтением и ack)
- **THEN** оно забирается `XAutoClaim` и обрабатывается заново тем же путём, что новые сообщения

#### Scenario: Messages survive consumer restart

- **WHEN** процесс консюмера перезапускается, оставив необработанные сообщения в PEL
- **THEN** после старта эти сообщения reclam'ятся и обрабатываются (at-least-once)

#### Scenario: StopConsumer waits for loop exit

- **WHEN** горутина цикла находится внутри обработки сообщения (например, `sink.Publish`) и
  вызван `StopConsumer`
- **THEN** `StopConsumer` SHALL не возвращаться, пока цикл не завершил текущую итерацию
  (done-канал закрыт); после его возврата вызывающий может закрывать sink без гонки с
  публикующей горутиной

#### Scenario: Restart after stop runs a single loop

- **WHEN** после `StopConsumer` на том же outbox-экземпляре снова вызван `StartConsumer`
- **THEN** запускается ровно один новый цикл (предыдущий завершён до возврата `StopConsumer`), а
  повторный `StartConsumer` без промежуточной остановки возвращает ошибку

Verified-by: pkg/meta/redis_outbox_test.go::TestInitConsumerGroupNoStream
Verified-by: pkg/meta/redis_outbox_test.go::TestReclaimPendingMessages
Verified-by: pkg/meta/redis_outbox_test.go::TestStopConsumerWaitsForLoopExit
Verified-by: pkg/meta/redis_outbox_test.go::TestRestartAfterStopSingleLoop
