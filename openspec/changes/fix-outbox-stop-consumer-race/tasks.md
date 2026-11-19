# Tasks: fix-outbox-stop-consumer-race

## 1. Done-channel семантика остановки

- [ ] 1.1 pkg/meta/redis_outbox.go: поле `done chan struct{}` в `RedisOutbox`; `StartConsumer`
      создаёт `done` перед `go o.consumeLoop(sink)`; `consumeLoop` — `defer close(o.done)`;
      `StopConsumer`: под `mu` — `cancel()` + `running=false` + локальная ссылка на `done`,
      вне `mu` — ожидание `<-done`, затем лог «Outbox consumer stopped». Поведение
      `IsHealthy`, poll/reclaim/спилла/backpressure не меняется.
      Файлы: pkg/meta/redis_outbox.go.
      Проверка: `go build ./pkg/meta/... && go vet ./pkg/meta/...`
- [ ] 1.2 Тесты (реальный Redis 127.0.0.1:6379/DB15, skip-паттерн `TestInitConsumerGroupNoStream`;
      новый мок `blockingSink` с каналами синхронизации):
      TestStopConsumerWaitsForLoopExit — событие в стриме, `Publish` блокирован горутиной,
      `StopConsumer` в отдельной горутине не возвращается, пока `Publish` не завершён; после
      release `StopConsumer` возвращается; TestRestartAfterStopSingleLoop — после `StopConsumer`
      повторный `StartConsumer` успешен и события не дублируются вторым циклом; повторный
      `StartConsumer` без промежуточной остановки возвращает ошибку. Прогон с `-race`.
      Файлы: pkg/meta/redis_outbox_test.go.
      Проверка: `go test -race -count=1 ./pkg/meta/... -run 'TestStopConsumerWaits|TestRestartAfterStop' -v`

## Definition of Done

- `go build ./...`
- `go vet ./pkg/meta/...`
- `go test -race -count=1 ./pkg/meta/... -run 'TestStopConsumerWaits|TestRestartAfterStop|TestReclaimPending|TestMoveToDeadQueue'` — зелёные
- `make test.meta.core`
- `golangci-lint run ./pkg/meta/...`
- `openspec validate fix-outbox-stop-consumer-race --strict` — без ошибок
