# Design: fix-meta-proxy-per-session-chroot

База: fork `agio-drive-v2` @`2d672403c` (в контексте — «adv2»).

## Architectural Context

- `MetaProxyServer` держит один общий движок (`grpc_server.go:49`). RPC `Chroot`
  (`grpc_server_admin.go:283-287`) вызывает `s.meta.Chroot(mctx, req.Subdir)` и
  возвращает только `&pb.ChrootResponse{Errno: uint32(errno)}`.
- `baseMeta.Chroot` идёт от текущего `m.root` (`base.go:2885`), создаёт отсутствующие
  каталоги (`Mkdir 0777`) и мутирует `m.root` без лока (`base.go:2905-2907`).
- Клиент: `x-session-id` отправляется, сервером игнорируется (`grpc_client.go:516-518`,
  `:538-542`); `NewSession` — один вызов (`cmd/mount.go:812`), при реконнекте не
  повторяется; `reChroot` строго до replay журнала (`grpc_client.go:710-716`;
  `ReplayWriteJournal` — `vfs.go:1423`, хук `mount.go:829`).
- Маппинг статусов у клиента: `PermissionDenied` → `EACCES`;
  `FailedPrecondition`/`ResourceExhausted` → `EIO` (`grpc_client.go:832-839`).
- Authz: Chroot стоит в None-ветке (`authz_interceptor.go:229`); short-circuit только
  для `path == "/"` и `View` (`:625`); Write на `/` → deny (`:368-372`); денайлы на
  `Debugf` (`:172`, `:180`, `:195`).
- Service identity (adv2, change `add-meta-proxy-service-identity`):
  `oidc.IsServiceIdentity(ctx)` — ранний выход в `UnaryInterceptor` (`:132`), фильтр
  Readdir (`filterEntriesByAuthz`, `grpc_server_fuse.go`) пропускается,
  `extractUserIDFromOIDC` возвращает `("", nil)`. Номера строк нашей зоны на adv2 не
  сдвинулись.
- Двойное path-пространство под chroot (A5): `inode_path_cache.go:47-55`, `:303-305`,
  `:261-275`; `inode_path_recovery.go:123`, `:175`; Readdir корня —
  `grpc_server_fuse.go:876-892`, `:905-909`.
- Разбор движка (A3): `CloseSession` — `base.go:1057-1073`, удаление сессии из redis —
  `redis.go:2985-3054`; сетевые ресурсы закрывает только `Shutdown()` —
  `redis.go:310-317`, `interface.go:516`.
- Утечка dir-хендлов: `s.handlers`, `grpc_server_dir_handler.go:150-159`.
- Graceful: бюджет 30s, `cmd/meta_proxy.go:396-411`. Базовый движок грузит формат в
  `meta_proxy.go:226`.
- Стриминговые RPC: `meta.proto:120-140`.

## Assumptions

| ID | Допущение | Статус |
|---|---|---|
| A1 | `stats.ConnTagInfo.RemoteAddr` в `TagConn` и `peer.Addr.String()` в interceptor — одна строка для одного соединения; открытые TCP-соединения имеют уникальные remote-адреса | перенос из v1; покрывается тестом на реальном TCP-листенере (не bufconn) |
| A2 | Топология: у клиента своё gRPC-соединение до прокси без L7-мультиплексирования HTTP/2; одно соединение на монтирование, при реконнекте — новое | ПОДТВЕРЖДЕНО 28.11: все активные VK-релизы drive-meta-proxy идут с `tls.enabled=true` → ssl-passthrough (`charts/drive-meta-proxy/templates/ingress.yaml:16-19`, `ingress-nginx.yaml:29-30`), TLS терминируется на поде, nginx — SNI/TCP; клиент и gateway — один `grpc.ClientConn` на процесс (`grpc_client.go:293`) |
| A3 | Разбор движка: `CloseSession` удаляет запись сессии из redis, отменяет sessCtx, ждёт sessWG; refresh вне WG асинхронно ≤1 Heartbeat (`base.go:932-938`, Heartbeat×5 — протухание); сетевые ресурсы закрывает только `Shutdown()`. `NewClient`+`Load(true)` дёшевы (горутин нет, пул диалится лениво), `NewSession` тяжёлый (~7–10 горутин + Lua + запись сессии) | ПОДТВЕРЖДЕНО |
| A4 | `Chroot` движка идёт от текущего `m.root`, создаёт каталоги и возвращает только errno; `Load(true)` обязателен до `Chroot` | факты explorer (v1) |
| A5 | Двойное path-пространство под chroot: кеш пинит inode 1 → `/`, `BuildChildPath` даёт `/name`, recovery строит полные пути; отсюда смешанный bulk-check Readdir и deny Write на `/` | ПОДТВЕРЖДЕНА |
| A6 | Маппинг статусов у клиента: `PermissionDenied` → `EACCES`; остальные — `EIO` (`grpc_client.go:832-839`) | факт explorer (v1) |
| A7 | `Chroot` на свежем движке допустим до `NewSession` (достаточно `Load(true)`); после незавершённого `NewSession` достаточно `Shutdown()` без `CloseSession` | ПОДТВЕРЖДЕНО 28.11 (explorer): Chroot зависит только от движка/формата/`m.root` (`base.go:2879-2907`), сессия не нужна; `NewSession(record bool)` — без ctx, sessCtx независим (`base.go:833`, `context.go:38-98`); CloseSession nil-safe (`base.go:1063-1069`), гонка CloseSession×незавершённый NewSession в нашей схеме не возникает (teardown ждёт `ready`, create синхронен). Уточнение: при провале `NewSession` зовём `CloseSession`+`Shutdown` (CloseSession гасит sessCtx и чистит redis-запись при sid>0); bare `Shutdown` оставил бы запись до TTL и фоновые горутины до смерти процесса |

## Component Map

| Компонент | Файл | Ответственность |
|---|---|---|
| `enginePool`, `poolEntry`, `EngineFactory` | `grpc_server_session.go` (new) | реестр по ключу, single-flight создание, refcount, teardown, shutdown |
| `proxySession`, `sessionRegistry`, `sessionStatsHandler`, session interceptors | `grpc_server_session.go` (new) | сессия = соединение, fail-closed, `sessionFor`, `bind`, `endSession` |
| `MetaProxyServer` | `grpc_server.go` | поля `pool`, `sessions`, `maxSessions`; `engineFor`; `CloseSessions`; поле `meta` удалено; 3-й аргумент фабрики |
| Chroot, NewSession, CloseSession | `grpc_server_admin.go` | обработчики v2 |
| Authz interceptor | `authz_interceptor.go` | Chroot → path-checked View (v1); Warn для денайлов; ctx в вызовах кеша |
| `InodePathCache` | `inode_path_cache.go` | ctx в `Get`/`BuildChildPath`; `rootPathFromContext` |
| Dir-хендлы | `grpc_server_dir_handler.go` | привязка к сессии, закрытие при завершении |
| Readdir / fuse RPC | `grpc_server_fuse.go` | ctx в вызовах кеша |
| Флаги, фабрика, graceful | `cmd/meta_proxy.go` | `--max-sessions`, фабрика с `Load(true)`, `CloseSessions` |

Не меняются: `InodePathCache` как процесс-global кеш, `pathResolver`, кеш решений authz,
`keyManager`, `volumeName`, STS, движки Redis/SQL/KV.

## Execution Flow

1. **Соединение открыто.** `TagConn` → `openSession(RemoteAddr)`: id, реестр,
   `session started`. При лимите — `session rejected`, сессии нет.
2. **Первый Meta-RPC сессии.** Session interceptor кладёт сессию в ctx →
   `engineFor(ctx)` → сессия ещё без движка → `pool.acquire(ctx, "", false)` → ленивое
   создание движка `""` → `bind`.
3. **Chroot RPC.** Authz interceptor (user identity, authz-режим): View на
   `"/"+key`. Затем обработчик:
   1. `sessionFor(ctx)` — fail-closed;
   2. `key = normalizeSubdir(req.Subdir)`;
   3. под `sess.chrootMu`: повтор с тем же key → сохранённый errno; другой key →
      `FailedPrecondition` (`session_already_chrooted`);
   4. authz-режим и user identity: lookup-проход цели на движке `""`
      (`ENOENT` → `target_missing`, `ENOTDIR` → `target_not_dir`, иное →
      `Unavailable`, `engine_error`, состояние не меняется);
   5. `pool.acquire(ctx, key, noCreate)` — при первом обращении создаёт движок;
      внутри создания выполняется финальный lookup (`noCreate`) и `Chroot(key)`;
   6. успех → `finishChroot` (финальный исход, `bind`), `chroot applied`,
      `{Errno: 0}`; errno → финальный errno, `chroot rejected`; ошибка →
      `Unavailable`, `engine_error`, состояние не меняется.
4. **Data-RPC.** `engineFor(ctx)` → `sess.entry.engine`.
5. **Реконнект.** Новое соединение → новая сессия → Chroot(key) → `acquire(key)`:
   `refs++` на живом движке либо новое создание.
6. **Закрытие соединения или RPC `CloseSession`.** `endSession`: dir-хендлы →
   `release(entry)` → при `refs == 0` запись снята → фоновый teardown.
7. **Остановка прокси.** `GracefulStop` → `CloseSessions(ctx, 30s)`:
   `endSession` для всех → `pool.shutdown(ctx)` → `Shutdown` базового движка.

## Key Code

Идентификаторы, помеченные «сверить», берутся из существующего кода adv2.

### Пул движков

```go
type EngineFactory func() (Engine, error) // NewClient + Load(true); сессию не открывает

const (
	engineCreateTimeout = 30 * time.Second // фабрика + Chroot + NewSession
	engineCloseTimeout  = 5 * time.Second  // CloseSession при teardown
)

type poolEntry struct {
	key     string // нормализованный subdir; "" — корень тома
	created time.Time

	ready chan struct{} // закрывается по завершении создания (успех или ошибка)
	// Поля ниже читаются только после закрытия ready.
	engine      Engine
	errno       uint32                 // Chroot свежего движка вернул errno
	err         error                  // фабрика или NewSession не удались
	newSessResp *pb.NewSessionResponse // ответ движкового NewSession для всей группы

	refs int // под enginePool.mu

	closeOnce    sync.Once // CloseSession движка
	shutdownOnce sync.Once // Shutdown движка
}

type enginePool struct {
	factory EngineFactory

	mu      sync.Mutex
	entries map[string]*poolEntry // не более одной записи на ключ
	closed  bool
}
```

```go
// acquire привязывает движок ключа к сессии (refs++). Первый приобретатель создаёт
// движок; остальные ждут того же создания (placeholder в самой map — single-flight).
func (p *enginePool) acquire(ctx context.Context, key string, noCreate bool) (*poolEntry, uint32, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, 0, status.Error(codes.Unavailable, "meta-proxy: engine pool is closed")
	}
	e, ok := p.entries[key]
	if !ok {
		e = &poolEntry{key: key, ready: make(chan struct{}), refs: 1}
		p.entries[key] = e // placeholder опубликован до начала создания
		p.mu.Unlock()
		p.create(e, noCreate)
		return p.settle(e)
	}
	e.refs++
	p.mu.Unlock()
	select {
	case <-e.ready:
	case <-ctx.Done():
		p.release(e)
		return nil, 0, status.FromContextError(ctx.Err()).Err()
	}
	return p.settle(e)
}

func (p *enginePool) settle(e *poolEntry) (*poolEntry, uint32, error) {
	if e.err != nil {
		p.release(e)
		return nil, 0, e.err
	}
	if e.errno != 0 {
		p.release(e)
		return nil, e.errno, nil
	}
	return e, 0, nil
}

// create строит движок вне мьютекса пула. Корень движка пишется ровно здесь.
func (p *enginePool) create(e *poolEntry, noCreate bool) {
	ctx, cancel := context.WithTimeout(context.Background(), engineCreateTimeout)
	defer cancel()
	defer close(e.ready)

	eng, err := p.factory()
	if err != nil {
		e.err = status.Errorf(codes.Unavailable, "meta-proxy: create engine for %q: %v", e.key, err)
		logger.Warnf("meta-proxy engine create failed subdir=%s err=%s", displaySubdir(e.key), err)
		p.discard(e)
		return
	}
	if e.key != "" {
		// authz-режим: цель обязана существовать. Финальный lookup на самом движке
		// сужает окно до Chroot, который умеет создавать каталоги.
		if noCreate {
			if errno, lerr := lookupSubdir(ctx, eng, e.key); lerr != nil || errno != 0 {
				eng.Shutdown()
				e.errno, e.err = uint32(errno), lerr
				p.discard(e)
				return
			}
		}
		if errno := eng.Chroot(ctx, e.key); errno != 0 { // сверить тип возврата Chroot
			eng.Shutdown()
			e.errno = uint32(errno)
			p.discard(e)
			return
		}
	}
	resp, err := newEngineSession(ctx, eng)
	if err != nil {
		eng.Shutdown() // CloseSession не зовём: сессия не установлена
		e.err = status.Errorf(codes.Unavailable, "meta-proxy: open engine session for %q: %v", e.key, err)
		logger.Warnf("meta-proxy engine create failed subdir=%s err=%s", displaySubdir(e.key), err)
		p.discard(e)
		return
	}
	e.engine, e.newSessResp, e.created = eng, resp, time.Now()
	logger.Infof("meta-proxy engine created subdir=%s", displaySubdir(e.key))
}

// discard снимает с реестра запись, чьё создание не удалось.
func (p *enginePool) discard(e *poolEntry) {
	p.mu.Lock()
	if p.entries[e.key] == e {
		delete(p.entries, e.key)
	}
	p.mu.Unlock()
}

// release отвязывает сессию (refs--). Запись снимается с реестра только здесь, под
// мьютексом и только при refs == 0 — после этого к записи никто не присоединится.
func (p *enginePool) release(e *poolEntry) {
	p.mu.Lock()
	if e.refs > 0 {
		e.refs--
	}
	last := e.refs == 0 && p.entries[e.key] == e
	if last {
		delete(p.entries, e.key)
	}
	p.mu.Unlock()
	if last {
		go p.teardown(e, engineCloseTimeout)
	}
}

// teardown: CloseSession движка ≤1 раза с бюджетом, затем Shutdown.
func (p *enginePool) teardown(e *poolEntry, budget time.Duration) {
	<-e.ready // запись могла быть снята ещё во время создания
	if e.engine == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.closeOnce.Do(func() { _ = e.engine.CloseSession() })
	}()
	timedOut := true
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
		timedOut = false
	case <-timer.C:
	}
	e.shutdownOnce.Do(func() { _ = e.engine.Shutdown() })
	uptime := time.Since(e.created).Round(time.Second)
	if timedOut {
		logger.Warnf("meta-proxy engine closed subdir=%s refs=0 uptime=%s close_timeout=true", displaySubdir(e.key), uptime)
	} else {
		logger.Infof("meta-proxy engine closed subdir=%s refs=0 uptime=%s", displaySubdir(e.key), uptime)
	}
}
```

```go
// normalizeSubdir — ключ пула: path.Clean("/"+subdir) без ведущего "/".
func normalizeSubdir(subdir string) string {
	return strings.TrimPrefix(path.Clean("/"+subdir), "/")
}

// displaySubdir — путь корня для логов: "/" для ключа "".
func displaySubdir(key string) string {
	if key == "" {
		return "/"
	}
	return "/" + key
}

// lookupSubdir проходит компоненты key от RootInode движка, ничего не создавая.
// Возвращает errno для ENOENT/ENOTDIR и ошибку для остальных сбоев.
func lookupSubdir(ctx context.Context, eng Engine, key string) (syscall.Errno, error) {
	ino := RootInode
	for _, name := range strings.Split(key, "/") {
		if name == "" {
			continue
		}
		var attr Attr // сверить сигнатуру Lookup и поля Attr по interface.go
		if err := eng.Lookup(ctx, ino, name, true, &attr); err != nil {
			if errors.Is(err, syscall.ENOENT) {
				return syscall.ENOENT, nil
			}
			if errors.Is(err, syscall.ENOTDIR) {
				return syscall.ENOTDIR, nil
			}
			return 0, status.Errorf(codes.Unavailable, "meta-proxy: lookup %q: %v", name, err)
		}
		if !attr.IsDir() {
			return syscall.ENOTDIR, nil
		}
		ino = attr.Inode
	}
	return 0, nil
}
```

В `create` блок `noCreate` намеренно стоит внутри `if e.key != ""`.

### Сессия

```go
type proxySession struct {
	srv   *MetaProxyServer
	id    uint64
	peer  string
	start time.Time

	chrootMu    sync.Mutex // сериализует Chroot RPC
	mu          sync.Mutex // защищает поля ниже
	entry       *poolEntry // движок пула; nil, пока не потребовался
	chroot      string     // key первого Chroot (для идемпотентного повтора)
	chrootErrno uint32     // errno первого Chroot
	root        string     // key успешного Chroot; "" — корень тома
	ended       bool
	handles     map[uint64]*dirHandlerEntry
}

// bind держит не более одной ссылки сессии на движок.
func (s *MetaProxyServer) bind(sess *proxySession, e *poolEntry) {
	sess.mu.Lock()
	var extra *poolEntry
	switch {
	case sess.ended, sess.entry == e:
		extra = e // ссылку из acquire сразу возвращаем
	case sess.entry == nil:
		sess.entry = e
	default:
		extra, sess.entry = sess.entry, e
	}
	sess.mu.Unlock()
	if extra != nil {
		s.pool.release(extra)
	}
}

func (s *MetaProxyServer) endSession(sess *proxySession) {
	sess.mu.Lock()
	if sess.ended {
		sess.mu.Unlock()
		return
	}
	sess.ended = true
	entry, handles, root := sess.entry, sess.handles, sess.root
	sess.entry, sess.handles = nil, nil
	sess.mu.Unlock()

	s.sessions.remove(sess)
	s.closeDirHandles(handles)
	if entry != nil {
		s.pool.release(entry)
	}
	logger.Infof("meta-proxy session closed id=%d chroot=%s uptime=%s",
		sess.id, displaySubdir(root), time.Since(sess.start).Round(time.Second))
}

// engineFor — единственный путь обработчиков к движку.
func (s *MetaProxyServer) engineFor(ctx context.Context) (Engine, error) {
	sess, err := s.sessionFor(ctx)
	if err != nil {
		return nil, err
	}
	e := s.sessionEntry(sess, ctx) // лениво приобретает движок "" при необходимости
	...
	return e.engine, nil
}
```

`sessionStatsHandler` (как в v1): `TagConn` → `openSession(info.RemoteAddr)`;
`HandleConn(*stats.ConnEnd)` → `endSessionByPeer`. Session interceptor стоит в цепочке
первым — authz interceptor видит сессию через ctx. `sessionFor` при отсутствии сессии
возвращает `ResourceExhausted`, если реестр на лимите, иначе `Internal`.

### Обработчик Chroot

```go
func (s *MetaProxyServer) Chroot(ctx context.Context, req *pb.ChrootRequest) (*pb.ChrootResponse, error) {
	sess, err := s.sessionFor(ctx)
	if err != nil {
		return nil, err
	}
	key := normalizeSubdir(req.Subdir)
	user := chrootLogUser(ctx)

	sess.chrootMu.Lock()
	defer sess.chrootMu.Unlock()

	sess.mu.Lock()
	done, prevKey, prevErrno := sess.chroot != "", sess.chroot, sess.chrootErrno
	sess.mu.Unlock()
	if done {
		if prevKey == key {
			return &pb.ChrootResponse{Errno: prevErrno}, nil
		}
		logger.Warnf("meta-proxy chroot rejected id=%d user=%s subdir=%s reason=session_already_chrooted",
			sess.id, user, displaySubdir(key))
		return nil, status.Error(codes.FailedPrecondition, "meta-proxy: session is already chrooted")
	}

	noCreate := s.authzEnabled && !oidc.IsServiceIdentity(ctx)
	if noCreate && key != "" {
		errno, lerr := s.checkChrootTarget(s.metaCtx(ctx, req.Ctx), sess, key)
		if lerr != nil {
			s.logChrootRejected(sess.id, user, key, "engine_error")
			return nil, lerr
		}
		if errno != 0 {
			s.finishChroot(sess, key, errno, nil)
			s.logChrootRejected(sess.id, user, key, chrootRejectReason(errno))
			return &pb.ChrootResponse{Errno: uint32(errno)}, nil
		}
	}

	e, errno, err := s.pool.acquire(ctx, key, noCreate)
	if err != nil {
		s.logChrootRejected(sess.id, user, key, "engine_error")
		return nil, err
	}
	if errno != 0 {
		s.finishChroot(sess, key, errno, nil)
		s.logChrootRejected(sess.id, user, key, chrootRejectReason(errno))
		return &pb.ChrootResponse{Errno: errno}, nil
	}
	s.finishChroot(sess, key, 0, e)
	logger.Infof("meta-proxy chroot applied id=%d user=%s subdir=%s", sess.id, user, displaySubdir(key))
	return &pb.ChrootResponse{Errno: 0}, nil
}

func chrootRejectReason(errno uint32) string {
	switch syscall.Errno(errno) {
	case syscall.ENOENT:
		return "target_missing"
	case syscall.ENOTDIR:
		return "target_not_dir"
	}
	return "engine_error"
}

// user в логах: sub из OIDC; service для service identity; "-" без identity.
func chrootLogUser(ctx context.Context) string {
	if oidc.IsServiceIdentity(ctx) {
		return "service"
	}
	if sub, err := extractUserIDFromOIDC(ctx); err == nil && sub != "" {
		return sub
	}
	return "-"
}
```

`finishChroot(sess, key, errno, e)`: под `sess.mu` фиксирует `chroot = key`,
`chrootErrno = errno`; при `e != nil` — `root = key` и `bind(sess, e)`.

`checkChrootTarget` выполняет `lookupSubdir` на движке `""` сессии (`sessionEntry`).
`newEngineSession` вызывает `eng.NewSession(true)` — единственный аргумент движкового
NewSession это `record bool` (`interface.go:521-522`; текущий обработчик
`grpc_server_lifecycle.go:67-84` читает только `req.Record`, ctx в движок не
передаётся) — и строит `pb.NewSessionResponse{Errno, Sid}` тем же кодом, что текущий
обработчик (type-assert `*redisMeta`, иначе Sid=0). Ответ сохраняется в
`poolEntry.newSessResp` и клонируется на каждый RPC `NewSession` группы. При провале
`NewSession` в `create`: `CloseSession()` затем `Shutdown()` — CloseSession гасит
sessCtx (останавливаются фоновые горутины) и чистит redis-запись при sid>0
(`base.go:1057-1073`); bare `Shutdown` оставил бы запись до TTL и горутины до смерти
процесса.

```go
func (s *MetaProxyServer) NewSession(ctx context.Context, _ *pb.NewSessionRequest) (*pb.NewSessionResponse, error) {
	e, err := s.sessionEntryFor(ctx) // fail-closed + ленивый движок ""
	if err != nil {
		return nil, err
	}
	return proto.Clone(e.newSessResp).(*pb.NewSessionResponse), nil
}

func (s *MetaProxyServer) CloseSession(ctx context.Context, _ *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	sess, err := s.sessionFor(ctx)
	if err != nil {
		return nil, err
	}
	s.endSession(sess) // CloseSession движка — только при teardown
	return &pb.CloseSessionResponse{}, nil
}
```

### Привязка в cmd

```go
// cmd/meta_proxy.go
newEngine := func() (meta.Engine, error) {
	eng, err := meta.NewClient(<те же аргументы, что у базового движка>)
	if err != nil {
		return nil, err
	}
	if err := eng.Load(true); err != nil { // формат обязателен до Chroot
		eng.Shutdown()
		return nil, err
	}
	return eng, nil
}
proxy := meta.NewMetaProxyServer(<два существующих аргумента>, newEngine)
```

`--max-sessions` (default 256) передаётся в сервер так же, как в v1. В graceful-пути
(`cmd/meta_proxy.go:396-411`): `GracefulStop()` → `CloseSessions(ctx)` с бюджетом 30s →
`Shutdown()` базового движка.

### InodePathCache и ctx

```go
func rootPathFromContext(ctx context.Context) string {
	if sess := sessionFromContext(ctx); sess != nil {
		return sess.rootPath() // "/" + root
	}
	return "/"
}
```

`InodePathCache.Get` и `BuildChildPath` получают ctx первым параметром; 23 call-site в
`authz_interceptor.go`, `grpc_server_fuse.go`, `grpc_server_dir_handler.go`,
`grpc_server_admin.go` (включая `Clone`) передают ctx запроса — как в v1.

## Invariants

- **I1.** Каждый Meta/STS/стриминговый RPC привязан ровно к одной сессии; без сессии
  движок не вызывается.
- **I2.** Корень движка пула пишется ровно один раз — при создании в
  `enginePool.create`; ни один RPC не вызывает `Chroot` на опубликованном движке.
- **I3.** В реестре не более одной записи на ключ; пока запись в реестре и создание
  удалось, `refs >= 1`.
- **I4.** Сессия держит не более одной ссылки ровно на одну запись.
- **I5.** Teardown (`CloseSession` ≤1, `Shutdown` = 1) выполняется тогда и только
  тогда, когда запись покидает реестр с `refs == 0`, либо при остановке пула.
- **I6.** `NewSession` движка — ровно один раз за жизнь движка; `CloseSession` — не
  более одного раза.
- **I7.** Chroot пользовательского identity в authz-режиме не создаёт каталоги
  (pre-check на движке `""` + финальный lookup на свежем движке).
- **I8.** Обработчики обращаются к движкам только через пул; базовый движок — только
  через `pathResolver`.
- **I9.** `InodePathCache` хранит только полные пути тома; inode 1 → корень сессии.

## Decisions & Rationale

- **D1. Сессия = gRPC-соединение** (как в v1). Соединение — единственный признак,
  который клиент не подделывает и который сервер наблюдает без изменения протокола.
- **D2. Пул движков по уникальному chroot** — принят (решение владельца R1).
  Per-session движки отклонены: профиль ресурсов был бы пропорционален числу
  клиентов, а не компаний. Альтернативы B (мэппинг путей/inode на уровне прокси без
  отдельных движков) и C (общий движок с мьютексом вокруг Chroot) отклонены как в v1:
  первая затрагивает каждый RPC, вторая не даёт изоляции.
- **D3. Single-flight через placeholder в map.** Держать мьютекс на время создания
  нельзя — медленный redis блокировал бы закрытие соединений. Отдельная группа
  `singleflight` дублировала бы состояние «кто создаёт ключ». Placeholder даёт то же
  поведение без новой зависимости.
- **D4. Движковый `NewSession` — при создании, не лениво.** Клиент не повторяет
  `NewSession` при реконнекте; ленивая схема оставила бы пересозданный движок без
  сессии. Дополнительный плюс — опубликованный движок всегда готов, RPC не платит за
  инициализацию.
- **D5. Аргументы движковой сессии фиксированы.** Движок общий для группы, аргументы
  первого клиента сделали бы поведение группы зависимым от порядка прихода.
- **D6. RPC `CloseSession` завершает только сессию отправителя.** Иначе один клиент
  закрывал бы сессию движка всей группе.
- **D7. Teardown × acquire.** Снятие с реестра только под мьютексом при `refs == 0`
  либо при провале создания. Опоздавшие получают новый движок; сосуществование старого
  и нового безопасно (разные sid).
- **D8. Без `--max-engines`.** Движок живёт, пока у него есть сессия, значит движков
  не больше, чем сессий; отдельный флаг — вторая ручка и ещё один отказ без пользы.
- **D9. Pre-check цели.** На движке `""` до приобретения целевого движка (authz-режим,
  user identity) + финальный lookup на свежем движке. Service identity идёт мимо
  authz-поверхности, поэтому для неё семантика «как сегодня».
- **D10. Поле `user` в логах.** `sub` | `service` | `-` — три различимых состояния.
- **D11. Базовый движок — только `pathResolver`.** Удаление поля `meta` превращает
  обращение обработчиков к базовому движку в ошибку компиляции.
- **D12. Деплой-линия — adv2** (решение владельца R2).

## Integration Points

- gRPC `MetaService`: Meta-RPC, `Chroot`, `NewSession`, `CloseSession`, стриминги.
- Интерфейс движка: `Load(true)`, `Chroot`, `NewSession`, `CloseSession`, `Shutdown`,
  `Lookup`.
- Authz: interceptor, `CheckPermission`, `AuthzPermissionView`; OIDC:
  `extractUserIDFromOIDC`, `oidc.IsServiceIdentity`.
- `InodePathCache`; `pathResolver`; `keyManager`; STS; логгер `pkg/meta`.
- Redis: записи сессий движков (штатный механизм).
- agio-drive CI (`JUICEFS_REF`, PIN) и линия образа прокси — вне этого change.

## Migration Plan

1. **Этот change.** Реализация в worktree на базе `agio-drive-v2` @`2d672403c`,
   архивация после реализации.
2. **[GO-1, этап релиза] Перенос darwin-CI коммитов.** Влить коммиты main
   `7c1d4ca82`, `7062e02e7` в `agio-drive-v2` (или эквивалент). Отдельное решение.
3. **[GO-2, этап релиза] agio-drive CI.** `JUICEFS_REF=agio-drive-v2` + новый PIN
   (коммит adv2 после шага 2 и этого change). Отдельное решение.
4. **[GO-3, этап релиза] Линия сборки образа прокси** переводится на adv2. Отдельное
   решение.
5. **Порядок с соседними change.** `add-meta-proxy-service-identity` первым (его код
   уже в базе), затем этот change; `add-gateway-platform-mode`, `implement-encrypt`,
   `implement-windows-mount` не зависят от нас — любой порядок, но ребейз на актуальную
   базу перед архивацией каждого.
6. **Выкатка.** Рестарт прокси; новый флаг `--max-sessions` (default 256); миграции
   данных нет. Клиенты переподключаются, их Chroot применяется заново через RPC.
7. **Откат.** Возврат предыдущего образа прокси; постоянного состояния прокси не
   меняет, записи сессий в redis протухают штатно.

## Open Questions (закрыты верификацией 28.11)

- **OQ1 — закрыт.** Клиент: `Load(true)` внутри делает `NewSession(false)` (до
  Chroot, `grpc_client_lifecycle.go:62`), затем Chroot (`cmd/mount.go:803`) и
  `NewSession(true)` (`cmd/mount.go:812`) — sid перезаписывается; сохраняется в
  `m.sid` (`grpc_client_lifecycle.go:81`), шлётся телом (`MetaContext.SessionId`,
  `grpc_client.go:583,595`) и заголовком `x-session-id` (`:541-542`); прокси его не
  использует. Реконнект: только `FlushSession` → reChroot → replay, NewSession не
  повторяется. Оба RPC `NewSession` получают клон сохранённого ответа своего движка —
  дизайн совместим. До Chroot клиент на миг сидит на движке `""` — штатно для пула.
- **OQ2 — закрыт (риск документирован, фолбэк не требуется).** sid-ключёвка: локи —
  поля `sid_owner` в `lockf/lockp` + индекс `locked$sid` (`redis.go:710-712`,
  `:673-675`); sustained-иноды `session$sid` (unlink Nlink==0, truncate,
  rename-перезапись — `redis.go:669-671`, `:1869-1905`, `:2137`, `:2283-2284`,
  `:2658`, `:2748`); запись сессии (`redis.go:462-489`). usedSpace/delfiles — не по
  sid. Разрушитель — `doCleanStaleSession` (`redis.go:2990-3036`) — срабатывает
  только при CloseSession группы (refs==0 — клиентов группы уже нет, корректная
  точка) или при реально протухшей записи; живая группа heartbeat'ится. Внутри
  группы клиенты различаются owner-строкой. Сравнение с today (общий движок на всех
  тенантов): поведение то же или лучше — сегодня stale-локи/sustained живут до
  рестарта прокси. Фолбэк «движок только при пустой группе» — отдельный change при
  необходимости.
- **OQ3 — закрыт решением D9.** Pre-check к service identity не применяется
  (gateway в platform-режиме Chroot не делает вовсе — OQ5).
- **OQ4 — закрыт.** VK: ssl-passthrough на всех активных релизах
  (`charts/drive-meta-proxy/templates/ingress.yaml:16-19`; values
  stage/vector-stage/hector-stage/tst-enc-vk — `tls.enabled=true`;
  `agio-cloud/clouds/vk/k8s/values/kube-system/ingress-nginx.yaml:29-30`; L4 TCP
  vkcs lb — `agio-terraform-vk/lb-k8s-ingress.tf:23-50`). L7-мультиплексирования
  нет. YC-инсталляция (TLS на nginx, backend-protocol GRPC) законсервирована.
- **OQ5 — закрыт.** Gateway: один gRPC-коннект на процесс = одна сессия прокси
  (`cmd/gateway.go:276-322`, `grpc_client.go:293`); порядок Load→Chroot→NewSession
  как у mount; legacy-режим полагается на неявное создание каталогов
  (`base.go:2885-2888`), platform-режим — `Chroot("")` no-op
  (`cmd/gateway_platform.go:100-102`, `base.go:2880`) — сидит на движке `""`.
- **OQ6 — закрыт** (см. A7).
- **OQ7 — закрыт.** `pb.NewSessionRequest{Record bool}`,
  `pb.NewSessionResponse{Errno uint32, Sid uint64}`; обработчик читает только
  `req.Record` (`grpc_server_lifecycle.go:67-84`); Sid — type-assert `*redisMeta`,
  иначе 0; сигнатура `NewSession(record bool) error` (`interface.go:521-522`).
- **OQ8 — закрыт.** sessCtx = собственный `Background()`+cancel (`base.go:833`,
  `context.go:38-98`); от переданного в NewSession ctx не зависит ни в какую
  сторону.
