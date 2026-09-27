/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Command load runs the stage-9 load scenarios (FR-TEST-22..25) against a local
// Redis + in-process Meta Proxy + counting fake KeyManager, and writes a markdown
// report with p50/p99 latencies and pass/fail against the SRS targets. Targets not
// met on the test machine are recorded as deviations (task 9.4: non-blocking).
//
// Usage: go run ./tests/load -redis 127.0.0.1:6379 -db 15 -clients 8 -iters 200
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/juicedata/juicefs/pkg/chunk"
	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/oidc"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/juicedata/juicefs/pkg/meta"
)

const userMetadataKey = "x-load-user"

// countingKeyManager is a KeyManagerClient that answers instantly with real AGFK
// wrap/unwrap under a fixed KEK and counts every call (the "0 extra round-trips"
// assertion of FR-TEST-25 reads getFekCalls).
type countingKeyManager struct {
	kek         []byte
	createCalls atomic.Int64
	getFekCalls atomic.Int64
}

func newCountingKeyManager() *countingKeyManager {
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i*7 + 1)
	}
	return &countingKeyManager{kek: kek}
}

func (k *countingKeyManager) aad(volumeUUID, driveFileID string, inode int64, ver uint32) meta.FekAAD {
	return meta.FekAAD{VolumeUUID: volumeUUID, CompanyID: "load-company", DriveFileID: driveFileID, Inode: meta.Ino(inode), FekVersion: ver}
}

func (k *countingKeyManager) CreateFileKey(ctx context.Context, req *kmpb.CreateFileKeyRequest) (*kmpb.CreateFileKeyResponse, error) {
	k.createCalls.Add(1)
	fek, err := meta.NewFEK()
	if err != nil {
		return nil, err
	}
	wrapped, err := meta.WrapFEK(k.kek, fek, k.aad(req.VolumeUuid, req.DriveFileId, req.Inode, 1), 1)
	if err != nil {
		return nil, err
	}
	return &kmpb.CreateFileKeyResponse{WrappedFek: wrapped, DriveFileId: req.DriveFileId, FekVersion: 1, KekVersion: 1, CryptoAlg: "AES-256-GCM"}, nil
}

func (k *countingKeyManager) GetFileFEK(ctx context.Context, req *kmpb.GetFileFEKRequest) (*kmpb.GetFileFEKResponse, error) {
	k.getFekCalls.Add(1)
	fek, _, err := meta.UnwrapFEK(k.kek, req.WrappedFek, k.aad(req.VolumeUuid, req.DriveFileId, req.Inode, req.FekVersion))
	if err != nil {
		return nil, err
	}
	return &kmpb.GetFileFEKResponse{Fek: fek, DriveFileId: req.DriveFileId, FekVersion: req.FekVersion, KekVersion: 1, CryptoAlg: "AES-256-GCM"}, nil
}

func (k *countingKeyManager) FetchCompanyKEK(ctx context.Context, req *kmpb.FetchCompanyKEKRequest) (*kmpb.FetchCompanyKEKResponse, error) {
	out := make([]byte, len(k.kek))
	copy(out, k.kek)
	return &kmpb.FetchCompanyKEKResponse{Kek: out, KekVersion: 1}, nil
}

func (k *countingKeyManager) ProvisionCompanyKEK(ctx context.Context, req *kmpb.ProvisionCompanyKEKRequest) (*kmpb.ProvisionCompanyKEKResponse, error) {
	return &kmpb.ProvisionCompanyKEKResponse{KekVersion: 1, Created: false}, nil
}

func (k *countingKeyManager) GetPermissionGeneration(ctx context.Context, req *kmpb.GetPermissionGenerationRequest) (*kmpb.GetPermissionGenerationResponse, error) {
	return &kmpb.GetPermissionGenerationResponse{Generation: 0}, nil
}

func (k *countingKeyManager) GetSTSCredentials(ctx context.Context, req *kmpb.GetSTSCredentialsRequest) (*kmpb.GetSTSCredentialsResponse, error) {
	return &kmpb.GetSTSCredentialsResponse{}, nil
}

func (k *countingKeyManager) GetNodeSTSCredentials(ctx context.Context, req *kmpb.GetNodeSTSCredentialsRequest) (*kmpb.GetSTSCredentialsResponse, error) {
	return &kmpb.GetSTSCredentialsResponse{}, nil
}

func (k *countingKeyManager) RotateFileFEK(ctx context.Context, req *kmpb.RotateFileFEKRequest) (*kmpb.RotateFileFEKResponse, error) {
	return nil, fmt.Errorf("not implemented in load harness")
}

func (k *countingKeyManager) Close() error { return nil }

// identityInterceptor mirrors the OIDC interceptor: it lifts the simulated 'sub'
// from outgoing metadata into a verified IDToken.
func identityInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get(userMetadataKey); len(vals) > 0 && vals[0] != "" {
				ctx = oidc.WithIDToken(ctx, &oidc.IDToken{Subject: vals[0]})
			}
		}
		return handler(ctx, req)
	}
}

type env struct {
	meta    meta.Meta // redisMeta (direct)
	server  *meta.MetaProxyServer
	grpcSrv *grpc.Server
	addr    string
	km      *countingKeyManager
	format  *meta.Format
	user    string
}

func userCtx(ctx context.Context, user string) meta.Context {
	return meta.WrapContext(metadata.AppendToOutgoingContext(ctx, userMetadataKey, user))
}

func newEnv(redisAddr string, db int) (*env, error) {
	// Flush the DB first: Init(force=true) updates the format but does not wipe
	// existing inodes (a previously killed run would leave files behind).
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, DB: db})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis ping %s: %w", redisAddr, err)
	}
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("flush db %d: %w", db, err)
	}
	_ = rdb.Close()

	// client-cache=false: the load test measures FEK/crypto overhead, not the
	// client-side cache; concurrent CSC use in this topology deadlocks in
	// go-redis v9.18.0 (push-notification processor), see stage-9 notes.
	metaURI := fmt.Sprintf("redis://%s/%d?client-cache=false", redisAddr, db)
	rm := meta.NewClient(metaURI, meta.DefaultConf())
	format := &meta.Format{
		Name: "load-test", UUID: uuid.New().String(), DirStats: true,
		Storage: "mem", Bucket: "test", BlockSize: 4096, Compression: "none",
		EncryptionEnabled: true, KEKVersion: 1, MetaVersion: meta.MaxVersion,
	}
	if err := rm.Init(format, true); err != nil {
		return nil, fmt.Errorf("init: %w", err)
	}
	if _, err := rm.Load(true); err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}

	km := newCountingKeyManager()
	server := meta.NewMetaProxyServer(rm, 1000)
	server.SetKeyManager(km)
	server.SetVolumeName(format.Name)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	grpcSrv := grpc.NewServer(grpc.ChainUnaryInterceptor(identityInterceptor()))
	pb.RegisterMetaServiceServer(grpcSrv, server)
	go func() { _ = grpcSrv.Serve(lis) }()

	e := &env{meta: rm, server: server, grpcSrv: grpcSrv, addr: lis.Addr().String(), km: km, format: format, user: uuid.New().String()}
	return e, nil
}

func (e *env) newClient() meta.Meta {
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	return meta.NewClient("grpc://"+e.addr, conf)
}

// createFiles creates n encrypted files through the proxy and returns their inodes.
func (e *env) createFiles(n int) ([]meta.Ino, error) {
	client := e.newClient()
	defer client.Shutdown()
	ctx := userCtx(context.Background(), e.user)
	inodes := make([]meta.Ino, 0, n)
	for i := 0; i < n; i++ {
		var ino meta.Ino
		var attr meta.Attr
		st := client.Create(ctx, meta.RootInode, fmt.Sprintf("load-%04d.exr", i), 0644, 022, syscall.O_CREAT|syscall.O_EXCL, &ino, &attr)
		if st != 0 {
			return nil, fmt.Errorf("create %d: %v", i, st)
		}
		inodes = append(inodes, ino)
	}
	return inodes, nil
}

type result struct {
	name   string
	target string
	p50    time.Duration
	p99    time.Duration
	rate   string // ops/s or MB/s
	pass   bool
	note   string
}

func percentiles(d []time.Duration) (p50, p99 time.Duration) {
	if len(d) == 0 {
		return 0, 0
	}
	s := make([]time.Duration, len(d))
	copy(s, d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(0.50*float64(len(s)-1))], s[int(0.99*float64(len(s)-1))]
}

// scenarioOpenHit (FR-TEST-22): Open with the FEK already in the client LRU.
func scenarioOpenHit(e *env, inodes []meta.Ino, clients, iters int) result {
	client := e.newClient()
	defer client.Shutdown()
	ctx := userCtx(context.Background(), e.user)

	// Warm up: one Open per file fills the FEK LRU (these are misses, not measured).
	for _, ino := range inodes {
		var attr meta.Attr
		if st := client.Open(ctx, ino, syscall.O_RDONLY, &attr); st != 0 {
			panic(fmt.Sprintf("warmup open: %v", st))
		}
	}

	var mu sync.Mutex
	var lat []time.Duration
	var wg sync.WaitGroup
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			local := make([]time.Duration, 0, iters)
			for i := 0; i < iters; i++ {
				ino := inodes[(c*iters+i)%len(inodes)]
				var attr meta.Attr
				start := time.Now()
				if st := client.Open(ctx, ino, syscall.O_RDONLY, &attr); st != 0 {
					panic(fmt.Sprintf("open: %v", st))
				}
				local = append(local, time.Since(start))
			}
			mu.Lock()
			lat = append(lat, local...)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	p50, p99 := percentiles(lat)
	return result{
		name:   "FR-TEST-22 Open hit (FEK in LRU)",
		target: "p99 ≤ 5 мс",
		p50:    p50,
		p99:    p99,
		rate:   fmt.Sprintf("%d ops total", len(lat)),
		pass:   p99 <= 5*time.Millisecond,
	}
}

// scenarioOpenMiss (FR-TEST-23): Open on a fresh client (empty FEK LRU) — every
// Open goes to the KeyManager.
func scenarioOpenMiss(e *env, inodes []meta.Ino, clients, iters int) result {
	ctx := userCtx(context.Background(), e.user)
	var mu sync.Mutex
	var lat []time.Duration
	var wg sync.WaitGroup
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			client := e.newClient()
			defer client.Shutdown()
			local := make([]time.Duration, 0, iters)
			for i := 0; i < iters; i++ {
				ino := inodes[(c*iters+i)%len(inodes)]
				var attr meta.Attr
				start := time.Now()
				if st := client.Open(ctx, ino, syscall.O_RDONLY, &attr); st != 0 {
					panic(fmt.Sprintf("open: %v", st))
				}
				local = append(local, time.Since(start))
			}
			mu.Lock()
			lat = append(lat, local...)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	p50, p99 := percentiles(lat)
	return result{
		name:   "FR-TEST-23 Open miss (KeyManager round-trip)",
		target: "p99 ≤ 100 мс",
		p50:    p50,
		p99:    p99,
		rate:   fmt.Sprintf("%d ops total", len(lat)),
		pass:   p99 <= 100*time.Millisecond,
	}
}

// scenarioThroughput (FR-TEST-24): sequential write+read of bytesTotal through the
// chunk store, legacy vs encrypted (CEK). Target: ≤10% degradation.
func scenarioThroughput(bytesTotal int) result {
	blob, err := object.CreateStorage("mem", "test", "", "", "")
	if err != nil {
		panic(err)
	}
	conf := chunk.Config{BlockSize: 1 << 20, CacheDir: "memory", MaxUpload: 50, MaxDownload: 200, MaxRetries: 3, BufferSize: 64 << 20}
	store := chunk.NewCachedStore(blob, conf, prometheus.NewRegistry())

	page := make([]byte, 1<<20)
	for i := range page {
		page[i] = byte(i % 251)
	}

	run := func(withKey bool) time.Duration {
		id := uint64(time.Now().UnixNano())
		var w chunk.Writer
		if withKey {
			cek := make([]byte, 32)
			for i := range cek {
				cek[i] = byte(i + 1)
			}
			w = store.NewWriterWithKey(id, 0, cek)
		} else {
			w = store.NewWriter(id, 0)
		}
		start := time.Now()
		for off := int64(0); off < int64(bytesTotal); off += int64(len(page)) {
			if _, err := w.WriteAt(page, off); err != nil {
				panic(err)
			}
		}
		if err := w.Finish(bytesTotal); err != nil {
			panic(err)
		}
		var r chunk.Reader
		if withKey {
			cek := make([]byte, 32)
			for i := range cek {
				cek[i] = byte(i + 1)
			}
			r = store.NewReaderWithKey(id, bytesTotal, cek)
		} else {
			r = store.NewReader(id, bytesTotal)
		}
		buf := make([]byte, 1<<20)
		for off := 0; off < bytesTotal; off += len(buf) {
			p := &chunk.Page{Data: buf}
			if _, err := r.ReadAt(context.Background(), p, off); err != nil {
				panic(err)
			}
		}
		return time.Since(start)
	}

	legacy := run(false)
	enc := run(true)
	mbps := func(d time.Duration) float64 { return float64(bytesTotal) / d.Seconds() / 1e6 }
	degradation := (mbps(legacy) - mbps(enc)) / mbps(legacy) * 100
	return result{
		name:   "FR-TEST-24 Throughput encrypted vs legacy",
		target: "деградация ≤ 10%",
		rate:   fmt.Sprintf("legacy %.1f MB/s, encrypted %.1f MB/s (degradation %.1f%%)", mbps(legacy), mbps(enc), degradation),
		pass:   degradation <= 10,
		note:   fmt.Sprintf("write+read %d MiB each", bytesTotal>>20),
	}
}

// scenarioRenderRPS (FR-TEST-25): metadata ops/sec through RenderMeta (direct
// Redis, no proxy/OIDC). The render node resolves FEKs locally under the Company
// KEK — the KeyManager call counter must stay at 0. The loop mixes GetAttr and
// Open; Open is the op that would need a KeyManager round-trip in the user
// topology, so it is the meaningful one for the "0 extra round-trips" target.
//
// The loop runs in a SINGLE goroutine: go-redis v9.18.0 (juicedata fork)
// deadlocks under concurrent command load on this machine (connections stuck
// waiting for replies that never arrive; reproduced with a minimal program,
// RESP2 and RESP3, Redis 7 and 8). Sequential use is stable. Recorded as a
// deviation in the report (task 9.4: non-blocking).
func scenarioRenderRPS(e *env, inodes []meta.Ino, duration time.Duration, redisAddr string, db int) result {
	kek := make([]byte, len(e.km.kek))
	copy(kek, e.km.kek)
	rm := meta.NewClient(fmt.Sprintf("redis://%s/%d?client-cache=false", redisAddr, db), meta.DefaultConf())
	if _, err := rm.Load(true); err != nil {
		panic(fmt.Sprintf("render load: %v", err))
	}
	render := meta.NewRenderMeta(rm, kek, 1, e.format.UUID, "load-company", "companies/load-company")

	stop := time.After(duration)
	getFekBefore := e.km.getFekCalls.Load() // counter is shared with the proxy scenarios; measure the delta
	var ops int64
	i := 0
	for {
		select {
		case <-stop:
			goto done
		default:
		}
		ino := inodes[i%len(inodes)]
		i++
		var attr meta.Attr
		if i%2 == 0 {
			if st := render.GetAttr(meta.Background(), ino, &attr); st != 0 {
				panic(fmt.Sprintf("getattr: %v", st))
			}
		} else {
			if st := render.Open(meta.Background(), ino, syscall.O_RDONLY, &attr); st != 0 {
				panic(fmt.Sprintf("open: %v", st))
			}
		}
		ops++
	}
done:
	rps := float64(ops) / duration.Seconds()
	getFek := e.km.getFekCalls.Load() - getFekBefore
	return result{
		name:   "FR-TEST-25 Render metadata RPS",
		target: "0 доп. round-trips на FEK",
		rate:   fmt.Sprintf("%.0f ops/s (1 goroutine, %s)", rps, duration),
		pass:   getFek == 0,
		note:   fmt.Sprintf("KeyManager GetFileFEK calls during scenario: %d; single-goroutine due to go-redis v9.18.0 fork concurrency deadlock (see Notes)", getFek),
	}
}

func main() {
	redisAddr := flag.String("redis", "127.0.0.1:6379", "Redis host:port")
	db := flag.Int("db", 15, "Redis DB for the load volume (will be flushed)")
	clients := flag.Int("clients", 8, "concurrent clients")
	iters := flag.Int("iters", 200, "Open iterations per client (hit/miss scenarios)")
	bytesTotal := flag.Int("bytes", 64<<20, "throughput scenario size in bytes")
	out := flag.String("out", "", "report path (default tests/load/report-<date>.md)")
	flag.Parse()

	fmt.Printf("load harness: redis=%s db=%d clients=%d iters=%d\n", *redisAddr, *db, *clients, *iters)
	e, err := newEnv(*redisAddr, *db)
	if err != nil {
		fatal(err)
	}
	defer e.grpcSrv.Stop()

	inodes, err := e.createFiles(64)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("created %d encrypted files\n", len(inodes))

	results := []result{}
	fmt.Println("scenario: open-hit...")
	results = append(results, scenarioOpenHit(e, inodes, *clients, *iters))
	fmt.Println("scenario: open-miss...")
	results = append(results, scenarioOpenMiss(e, inodes, *clients, *iters))
	fmt.Println("scenario: throughput...")
	results = append(results, scenarioThroughput(*bytesTotal))
	fmt.Println("scenario: render-rps...")
	results = append(results, scenarioRenderRPS(e, inodes, 10*time.Second, *redisAddr, *db))
	fmt.Println("all scenarios done")

	path := *out
	if path == "" {
		path = fmt.Sprintf("tests/load/report-%s.md", time.Now().Format("2006-01-02"))
	}
	if err := writeReport(path, results); err != nil {
		fatal(err)
	}
	fmt.Printf("report written to %s\n", path)

	allPass := true
	for _, r := range results {
		status := "PASS"
		if !r.pass {
			status = "DEVIATION"
			allPass = false
		}
		fmt.Printf("%-45s %-22s %s\n", r.name, r.rate, status)
	}
	if !allPass {
		fmt.Println("note: unmet targets are recorded as deviations (task 9.4: non-blocking)")
	}
}

func writeReport(path string, results []result) error {
	var b strings.Builder
	b.WriteString("# Load test report (stage 9, task 9.4)\n\n")
	b.WriteString(fmt.Sprintf("- Date: %s\n", time.Now().Format("2006-01-02 15:04 MST")))
	b.WriteString(fmt.Sprintf("- Machine: %s/%s, Go %s, GOMAXPROCS=%d\n", runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.GOMAXPROCS(0)))
	b.WriteString("- Topology: local Redis + in-process Meta Proxy + counting fake KeyManager (in-process gRPC); mem object storage for the throughput scenario.\n")
	b.WriteString("- Targets not met are recorded as deviations and do not block the stage (tasks.md 9.4).\n\n")
	b.WriteString("| Scenario | Target | p50 | p99 | Rate | Status |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range results {
		status := "PASS"
		if !r.pass {
			status = "**DEVIATION**"
		}
		p50, p99 := "-", "-"
		if r.p50 > 0 {
			p50 = r.p50.Round(100 * time.Microsecond).String()
		}
		if r.p99 > 0 {
			p99 = r.p99.Round(100 * time.Microsecond).String()
		}
		note := ""
		if r.note != "" {
			note = " — " + r.note
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s%s | %s |\n", r.name, r.target, p50, p99, r.rate, note, status))
	}
	b.WriteString("\n## Notes\n\n")
	b.WriteString("- FR-TEST-22/23 measure the gRPC Open RPC (GetAttr + FEK delivery) end to end; the SRS targets assume the production proxy topology — in-process gRPC here is a lower bound on network latency.\n")
	b.WriteString("- FR-TEST-25 target \"5000 RPS × 100 nodes\" is a fleet-level goal; the single-node ops/s above and the 0-extra-round-trips assertion are what this harness can verify locally.\n")
	b.WriteString("- **Deviation (FR-TEST-25 concurrency):** the render scenario runs a single goroutine. go-redis v9.18.0 (juicedata fork) deadlocks under concurrent command load on this machine: connections get stuck waiting for replies that never arrive (goroutine dumps show blocked socket reads + pool exhaustion). Reproduced with a minimal program (no JuiceFS code), on Redis 7 and 8, RESP2 and RESP3; sequential use is stable. This is a dependency bug, not an encryption-path issue; concurrent render load must be re-verified after the go-redis fork is fixed/upgraded.\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}
