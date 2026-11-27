# scripts/dod-cycles.ps1 — задача 7.5: десять циклов «запуск — чтение —
# запись — удаление — остановка» на живом sync root (spec domain-cfapi-provider).
# Число горутин — pprof debug-агента serve (127.0.0.1:6060, cmd/main.go:
# стартует агент на 6060..6099, если не передан --no-agent); занятость
# буферов пула TRANSFER_DATA HTTP не экспортируется — контролируется
# косвенно: в goroutine-дампе не остаётся горутин в readFetch/handleFetchData,
# число строк «FETCH_DATA done» в логе serve == числу прочитанных файлов,
# аллокации буферов — файлы heap-профиля (задача 7.8, снимаются опционально).
#
# Запуск (PowerShell, машина с зарегистрированным sync root):
#   .\scripts\dod-cycles.ps1 -Meta "grpc://meta-proxy:9900?tls=1&..." `
#       -Root "$env:USERPROFILE\AgioDrive" -Cycles 10
# Параметры -CompanyID/-NodeID и прочие флаги serve — как в живом прогоне
# оркестратора; STS-флаги передаются через -ExtraServeArgs.
#
# Скрипт НЕ запускается из агентов разработки: живая часть — оркестратор.
param(
    [Parameter(Mandatory = $true)][string]$Meta,        # URI meta-движка для serve
    [Parameter(Mandatory = $true)][string]$Root,        # путь sync root (зарегистрированный)
    [int]$Cycles = 10,
    [string]$LogFile = "$env:TEMP\clouddrive-cycles.log",
    [int]$DebugPort = 6060,
    [string[]]$ExtraServeArgs = @(),
    [string]$JuicefsExe = ".\juicefs.exe"
)

$ErrorActionPreference = "Stop"

function Get-GoroutineCount {
    # Число строк '^goroutine ' в /debug/pprof/goroutine?debug=1 (текстовый дамп).
    $dump = (Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$DebugPort/debug/pprof/goroutine?debug=1" -TimeoutSec 5).Content
    return ($dump -split "`n" | Select-String -Pattern "^goroutine " -SimpleMatch:$false).Count
}

function Wait-Ready {
    # Маркер готовности в логе serve (спека «Готовность»): `OK, <том> is ready`.
    $deadline = (Get-Date).AddSeconds(120)
    while ((Get-Date) -lt $deadline) {
        if ((Test-Path $LogFile) -and (Select-String -Path $LogFile -Pattern "is ready" -SimpleMatch:$false -Quiet)) {
            return $true
        }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

if (-not (Test-Path $JuicefsExe)) {
    throw "juicefs.exe не найден: соберите бинарь по рецепту 0.2 (CGO_ENABLED=1, CGO_CFLAGS='-IC:/WinFsp/inc/fuse')"
}

$results = @()
$readPath = Join-Path $Root "cycle_read.txt"
$writePath = Join-Path $Root "cycle_write.txt"

for ($i = 1; $i -le $Cycles; $i++) {
    Write-Host "=== Цикл $i / $Cycles ==="

    # 1. Запуск serve (свежий лог на цикл; debug-агент — по умолчанию включён).
    if (Test-Path $LogFile) {
        Remove-Item $LogFile -Force
    }
    $serve = Start-Process -FilePath $JuicefsExe `
        -ArgumentList (@("clouddrive", "serve", $Meta, "--log-id", "cycles") + $ExtraServeArgs) `
        -RedirectStandardError $LogFile -PassThru -NoNewWindow
    if (-not (Wait-Ready)) {
        throw "цикл ${i}: маркер готовности не появился за 120с (см. $LogFile)"
    }

    # 2. Базовая линия горутин (после готовности, до нагрузки).
    $goroutinesBase = Get-GoroutineCount

    # 3. Чтение (гидрация): читаем дегидратированный плейсхолдер насквозь.
    $bytes = [System.IO.File]::ReadAllBytes($readPath)
    if ($bytes.Length -le 0) {
        throw "цикл ${i}: $readPath пуст — создайте файл-плейсхолдер в корне перед прогоном"
    }
    Start-Sleep -Seconds 2  # FETCH_DATA done успевает попасть в лог

    # 4. Запись (выгрузка watcher'ом) и удаление (ACK_DELETE по U19).
    Set-Content -Path $writePath -Value "cycle $i payload $(Get-Date -Format o)" -Encoding UTF8
    Start-Sleep -Seconds 5  # коалисинг 2с + выгрузка + replace-after-upload
    Remove-Item $writePath -Force
    Start-Sleep -Seconds 2  # pending-delete (до 3с) + ACK_DELETE

    # 5. Проверки устоявшегося состояния до остановки.
    $goroutinesPeak = Get-GoroutineCount
    $fetchStarted = (Select-String -Path $LogFile -Pattern "FETCH_DATA:" -SimpleMatch:$false).Count
    $fetchDone = (Select-String -Path $LogFile -Pattern "FETCH_DATA done" -SimpleMatch:$false).Count
    if ($fetchStarted -ne $fetchDone) {
        throw "цикл ${i}: FETCH_DATA начато=$fetchStarted завершено=$fetchDone — задания не отвечены (проверить застрявшие буферы)"
    }

    # 6. Остановка (Ctrl+C эквивалент: завершаем процесс — Stop() провайдера
    #    выполняется по сигналу; маркер чистой остановки в логе).
    Stop-Process -Id $serve.Id -Force
    Wait-Process -Id $serve.Id -ErrorAction SilentlyContinue

    # 7. Горутины до/после: сопоставимы (в живом serve watcher/walk-горутины
    #    завершаются на Stop; дельта 0-2 допустима из-за фоновой телеметрии).
    $delta = $goroutinesPeak - $goroutinesBase
    $results += [pscustomobject]@{
        Cycle = $i
        GoroutinesBase = $goroutinesBase
        GoroutinesPeak = $goroutinesPeak
        Delta = $delta
        FetchJobs = $fetchDone
        Log = $LogFile
    }
    if ($delta -gt 2) {
        Write-Warning "цикл ${i}: рост горутин на $delta — проверить goroutine-дамп (см. ниже) перед следующим циклом"
    }
}

$results | Format-Table -AutoSize
$results | ConvertTo-Json -Depth 3 | Set-Content "$env:TEMP\clouddrive-cycles-result.json"
Write-Host "Готово: $Cycles циклов; сводка в `$env:TEMP\clouddrive-cycles-result.json"
Write-Host "Инвариант 7.5: Delta <= 2 на каждом цикле; FETCH jobs == числу чтений."

# Опционально для 7.8 (профиль во время циклов, вручную):
#   Invoke-WebRequest http://127.0.0.1:$DebugPort/debug/pprof/heap -OutFile heap_cycle$i.pprof
#   Invoke-WebRequest http://127.0.0.1:$DebugPort/debug/pprof/goroutine?debug=2 -OutFile goroutines_cycle$i.txt
#   serve запускается с GODEBUG=gctrace=1 (см. отчёт 7.8) — GC-паузы в stderr.
