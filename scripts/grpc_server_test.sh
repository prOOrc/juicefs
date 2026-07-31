#!/bin/bash
# grpc_server_test.sh - Запуск gRPC сервера для тестирования

set -e

PORT=${1:-9001}
BACKEND=${2:-"memkv://test"}

echo "Starting gRPC server on port $PORT with backend: $BACKEND"

# Запуск gRPC сервера в фоне
go run ./cmd/juicefs/meta_test.go grpc-server --addr :$PORT --backend "$BACKEND" &
SERVER_PID=$!

echo "gRPC server started with PID: $SERVER_PID"

# Ждем пока сервер запустится
sleep 2

# Проверяем что сервер запущен
if kill -0 $SERVER_PID 2>/dev/null; then
    echo "Server is running"
else
    echo "Server failed to start"
    exit 1
fi

# Ожидаем сигнал о завершении
trap "kill $SERVER_PID 2>/dev/null; exit" INT TERM EXIT

wait $SERVER_PID