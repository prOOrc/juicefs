#!/bin/bash
# grpc_e2e_test.sh - End-to-end тест для gRPC meta backend
# 
# Сценарий:
# 1. Запуск gRPC proxy сервера с memkv бэкендом
# 2. Форматирование ФС через gRPC
# 3. Монтирование ФС через gRPC
# 4. Тестовые операции (создание, чтение, удаление файлов)
# 5. Размонтирование и очистка

set -e

PORT=${1:-9561}
MOUNT_POINT="/tmp/juicefs-grpc-test-$$"
BACKEND="memkv://grpc-e2e-test"

cleanup() {
    echo "=== Cleanup ==="
    # Размонтировать ФС
    if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
        echo "Unmounting $MOUNT_POINT..."
        umount "$MOUNT_POINT" 2>/dev/null || true
    fi
    
    # Убить сервер
    if [ -n "$SERVER_PID" ]; then
        echo "Killing server (PID: $SERVER_PID)..."
        kill $SERVER_PID 2>/dev/null || true
        wait $SERVER_PID 2>/dev/null || true
    fi
    
    # Удалить точку монтирования
    rm -rf "$MOUNT_POINT"
    
    # Очистить memkv persistence
    rm -f /tmp/juicefs.grpc-e2e-test.setting.json
    
    echo "Cleanup done"
}

trap cleanup EXIT INT TERM

JFS_BINARY="./juicefs"

# Проверяем наличие бинарника, если нет - собираем
if [ ! -f "$JFS_BINARY" ]; then
    echo "=== Building juicefs binary ==="
    make juicefs
fi

echo "=== Starting gRPC E2E Test ==="
echo "Port: $PORT"
echo "Mount point: $MOUNT_POINT"
echo "Backend: $BACKEND"

# Создаем точку монтирования
mkdir -p "$MOUNT_POINT"

# Запускаем gRPC proxy сервер
echo "=== Starting gRPC proxy server ==="
$JFS_BINARY meta-proxy --meta-backend "$BACKEND" --addr ":$PORT" &
SERVER_PID=$!
echo "Server PID: $SERVER_PID"

# Ждем пока сервер запустится
sleep 2

# Проверяем что сервер запущен
if ! kill -0 $SERVER_PID 2>/dev/null; then
    echo "ERROR: Server failed to start"
    exit 1
fi
echo "Server is running"

# Форматируем ФС через gRPC
echo "=== Formatting filesystem via gRPC ==="
$JFS_BINARY format --debug --storage file --bucket "/tmp/jfs-grpc-data-$$" grpc://localhost:$PORT test-grpc-fs

# Монтируем ФС через gRPC
echo "=== Mounting filesystem via gRPC ==="
$JFS_BINARY mount grpc://localhost:$PORT "$MOUNT_POINT" --debug &
MOUNT_PID=$!
echo "Mount PID: $MOUNT_PID"

# Ждем пока смонтируется
sleep 3

# Проверяем что ФС смонтировано
if ! mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
    echo "ERROR: Failed to mount filesystem"
    kill $MOUNT_PID 2>/dev/null || true
    exit 1
fi
echo "Filesystem is mounted"

# Тестовые операции
echo "=== Testing file operations ==="

# Создание файла
echo "Creating test file..."
echo "Hello, gRPC!" > "$MOUNT_POINT/test.txt"

# Чтение файла
echo "Reading test file..."
CONTENT=$(cat "$MOUNT_POINT/test.txt")
if [ "$CONTENT" != "Hello, gRPC!" ]; then
    echo "ERROR: Content mismatch"
    exit 1
fi
echo "Content verified: $CONTENT"

# Создание директории
echo "Creating directory..."
mkdir "$MOUNT_POINT/testdir"

# Перемещение файла
echo "Moving file..."
mv "$MOUNT_POINT/test.txt" "$MOUNT_POINT/testdir/test.txt"

# Проверка перемещения
if [ ! -f "$MOUNT_POINT/testdir/test.txt" ]; then
    echo "ERROR: File not found after move"
    exit 1
fi
echo "File moved successfully"

# Удаление
echo "Cleaning up test files..."
rm -rf "$MOUNT_POINT/testdir"

echo "=== All tests passed ==="

# Размонтируем
echo "=== Unmounting ==="
kill $MOUNT_PID 2>/dev/null || true
wait $MOUNT_PID 2>/dev/null || true
umount "$MOUNT_POINT" 2>/dev/null || true

echo "=== E2E Test completed successfully ==="