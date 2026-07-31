#!/bin/bash
# gRPC Meta Performance Benchmark
# Requires: juicefs meta-proxy running on port 9561

cd /home/iobukhov/github/juicefs

echo "========================================="
echo "gRPC Meta Performance Benchmark"
echo "========================================="
echo ""

# Check if proxy is running
if ! pgrep -f "meta-proxy.*9561" > /dev/null; then
    echo "ERROR: gRPC proxy not running on port 9561"
    echo "Start it with: ./juicefs meta-proxy --meta-backend redis://127.0.0.1:6379/5 --addr :9561"
    exit 1
fi

echo "Proxy: Running on port 9561"
echo ""

# Run benchmark
go run scripts/bench_grpc.go 2>&1 | grep -E '===|GetAttr|Lookup|calls|Waiting'

echo ""
echo "========================================="
echo "Redis Baseline (for comparison):"
echo "========================================="
go test -bench='BenchmarkRedis/(getattr|lookup)' -benchtime=200ms -run=^$ ./pkg/meta 2>&1 | grep -E 'BenchmarkRedis/getattr|BenchmarkRedis/lookup'
