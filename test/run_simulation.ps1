# OtelContext V5.4 Chaos Simulation
# Runs all 7 test services in-process (no new windows), hammers all endpoints.
# Requires OtelContext backend: .\start_dev.ps1

param(
    [Alias("Workers")]  [int]    $Parallel     = 10,   # concurrent HTTP workers
    [Alias("Burst")]    [int]    $RequestCount = 0,    # kept for CLI compat, ignored — always runs until Ctrl+C
    [int]    $DelayMs  = 10,                           # ms between requests per worker
    [string] $LogDir   = (Join-Path $PSScriptRoot "..\tmp\logs")
)

$ErrorActionPreference = "Stop"
$RootDir = Split-Path $PSScriptRoot -Parent
$TmpDir  = Join-Path $RootDir "tmp"

# ── Weighted endpoint list ───────────────────────────────────────────────────
# order fans out to all 7 services — highest weight for maximum telemetry volume
$Endpoints = @(
    @{ Url = "http://localhost:9001/order";    Method = "POST"; Weight = 6 }
    @{ Url = "http://localhost:9002/pay";      Method = "POST"; Weight = 2 }
    @{ Url = "http://localhost:9003/check";    Method = "POST"; Weight = 2 }
    @{ Url = "http://localhost:9004/validate"; Method = "POST"; Weight = 1 }
    @{ Url = "http://localhost:9007/notify";   Method = "POST"; Weight = 1 }
)
$PickList = @()
foreach ($ep in $Endpoints) { for ($i = 0; $i -lt $ep.Weight; $i++) { $PickList += $ep } }

Write-Host ""
Write-Host "======================================" -ForegroundColor Magenta
Write-Host "  OtelContext V5.4 Chaos Simulation" -ForegroundColor Magenta
Write-Host "======================================" -ForegroundColor Magenta
Write-Host "  Workers : $Parallel"
Write-Host "  Delay   : ${DelayMs}ms / worker"
Write-Host "  Mode    : Continuous (Ctrl+C to stop)"
Write-Host ""

# ── Build ────────────────────────────────────────────────────────────────────
Write-Host "[1/3] Building test services..." -ForegroundColor Yellow

if (-not (Test-Path $TmpDir)) { New-Item -ItemType Directory -Path $TmpDir -Force | Out-Null }
if (-not (Test-Path $LogDir)) { New-Item -ItemType Directory -Path $LogDir -Force | Out-Null }

$Services = @(
    @{ Name = "userservice";         Port = 9005 }
    @{ Name = "authservice";         Port = 9004 }
    @{ Name = "inventoryservice";    Port = 9003 }
    @{ Name = "shippingservice";     Port = 9006 }
    @{ Name = "notificationservice"; Port = 9007 }
    @{ Name = "paymentservice";      Port = 9002 }
    @{ Name = "orderservice";        Port = 9001 }
)

Push-Location $RootDir
foreach ($svc in $Services) {
    Write-Host ("  {0,-26}" -f $svc.Name) -NoNewline -ForegroundColor DarkGray
    go build -o "$TmpDir\$($svc.Name).exe" "./test/$($svc.Name)" 2>&1 | Out-Null
    Write-Host "built" -ForegroundColor DarkGray
}
Pop-Location
Write-Host "  All services built." -ForegroundColor Green

# ── Start services (no new windows) ─────────────────────────────────────────
Write-Host ""
Write-Host "[2/3] Starting services..." -ForegroundColor Yellow

$Procs = @()
foreach ($svc in $Services) {
    $exe    = "$TmpDir\$($svc.Name).exe"
    $stdout = "$LogDir\$($svc.Name).stdout"
    $stderr = "$LogDir\$($svc.Name).stderr"
    $proc   = Start-Process -FilePath $exe -NoNewWindow -PassThru `
                            -RedirectStandardOutput $stdout `
                            -RedirectStandardError  $stderr
    $Procs += $proc
    Write-Host ("  {0,-26} PID {1,6}  :{2}" -f $svc.Name, $proc.Id, $svc.Port) -ForegroundColor Green
}

Write-Host "  Waiting for services to bind ports..." -ForegroundColor DarkGray
Start-Sleep -Seconds 4

# ── Cleanup ──────────────────────────────────────────────────────────────────
function Stop-Simulation {
    Write-Host ""
    Write-Host "[cleanup] Stopping services..." -ForegroundColor Yellow
    foreach ($p in $Procs) {
        try { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } catch {}
    }
    Write-Host "  Done. Logs in: $LogDir" -ForegroundColor Green
}
$null = Register-EngineEvent -SourceIdentifier PowerShell.Exiting -Action { Stop-Simulation } -ErrorAction SilentlyContinue

# ── Shared state ─────────────────────────────────────────────────────────────
# Use a lock object + synchronized hashtable.
# NOTE: [System.Threading.Interlocked] cannot update hashtable values (no true ref),
#       so we use Monitor locking on a dedicated lock object instead.
$shared = [hashtable]::Synchronized(@{
    Total   = [long]0
    Success = [long]0
    Fail    = [long]0
    Running = $true
    Lock    = [System.Object]::new()
})

# ── Worker script ─────────────────────────────────────────────────────────────
$workerScript = {
    param($shared, $pickList, $delayMs, $burst)

    $rng = [System.Random]::new([System.Threading.Thread]::CurrentThread.ManagedThreadId)

    while ($shared.Running) {

        $ep   = $pickList[$rng.Next(0, $pickList.Count)]
        $isOk = $false

        try {
            $req = [System.Net.WebRequest]::Create($ep.Url)
            $req.Method        = $ep.Method
            $req.Timeout       = 8000
            $req.ContentLength = 0
            $resp = $req.GetResponse()
            $resp.Close()
            $isOk = $true
        } catch {
            # expected — chaos services intentionally fail
        }

        # Atomic update via Monitor lock (Interlocked can't write back into hashtable)
        [System.Threading.Monitor]::Enter($shared.Lock)
        $shared.Total++
        if ($isOk) { $shared.Success++ } else { $shared.Fail++ }
        [System.Threading.Monitor]::Exit($shared.Lock)

        if ($delayMs -gt 0) { Start-Sleep -Milliseconds $delayMs }
    }
}

# ── Start runspace pool ───────────────────────────────────────────────────────
Write-Host ""
Write-Host "[3/3] Running load ($Parallel workers, ${DelayMs}ms delay)..." -ForegroundColor Yellow
Write-Host "       Ctrl+C to stop." -ForegroundColor DarkGray
Write-Host ""

$pool = [System.Management.Automation.Runspaces.RunspaceFactory]::CreateRunspacePool(1, $Parallel)
$pool.Open()

$runspaces = @()
for ($i = 0; $i -lt $Parallel; $i++) {
    $ps = [System.Management.Automation.PowerShell]::Create()
    $ps.RunspacePool = $pool
    [void]$ps.AddScript($workerScript)
    [void]$ps.AddArgument($shared)
    [void]$ps.AddArgument($PickList)
    [void]$ps.AddArgument($DelayMs)
    [void]$ps.AddArgument($RequestCount)
    $handle = $ps.BeginInvoke()
    $runspaces += @{ PS = $ps; Handle = $handle }
}

# ── Stats loop ────────────────────────────────────────────────────────────────
$startTime = [datetime]::UtcNow
$lastTotal = [long]0

try {
    while ($true) {
        Start-Sleep -Milliseconds 1000

        $elapsed = ([datetime]::UtcNow - $startTime).TotalSeconds

        [System.Threading.Monitor]::Enter($shared.Lock)
        $total   = $shared.Total
        $ok      = $shared.Success
        $fail    = $shared.Fail
        [System.Threading.Monitor]::Exit($shared.Lock)

        $delta  = $total - $lastTotal
        $lastTotal = $total
        $rps    = if ($elapsed -gt 0) { [math]::Round($total / $elapsed, 1) } else { 0 }
        $errPct = if ($total -gt 0) { [math]::Round($fail / $total * 100, 1) } else { 0 }
        $errCol = if ($errPct -gt 20) { "Red" } elseif ($errPct -gt 5) { "Yellow" } else { "Green" }

        Write-Host ("  {0,6}s | Total: {1,7} | OK: {2,7} | Fail: {3,5} | Err: " -f `
            [math]::Round($elapsed, 0), $total, $ok, $fail) -NoNewline -ForegroundColor Cyan
        Write-Host ("{0,5}%" -f $errPct) -NoNewline -ForegroundColor $errCol
        Write-Host (" | {0,6} req/s | +{1}/s" -f $rps, $delta) -ForegroundColor Cyan

    }
}
finally {
    $shared.Running = $false

    foreach ($rs in $runspaces) {
        try { $rs.PS.EndInvoke($rs.Handle) } catch {}
        $rs.PS.Dispose()
    }
    $pool.Close(); $pool.Dispose()

    $elapsed = ([datetime]::UtcNow - $startTime).TotalSeconds
    $total   = $shared.Total
    $ok      = $shared.Success
    $fail    = $shared.Fail
    $errPct  = if ($total -gt 0) { [math]::Round($fail / $total * 100, 1) } else { 0 }
    $rps     = if ($elapsed -gt 0) { [math]::Round($total / $elapsed, 1) } else { 0 }
    $errCol  = if ($errPct -gt 20) { "Red" } elseif ($errPct -gt 5) { "Yellow" } else { "Green" }

    Write-Host ""
    Write-Host "======================================" -ForegroundColor Magenta
    Write-Host "  Simulation Complete" -ForegroundColor Magenta
    Write-Host "======================================" -ForegroundColor Magenta
    Write-Host ("  Duration  : {0:F1} s"  -f $elapsed)
    Write-Host ("  Requests  : {0}"        -f $total)
    Write-Host ("  Success   : {0}"        -f $ok)   -ForegroundColor Green
    Write-Host ("  Failed    : {0}"        -f $fail)  -ForegroundColor Red
    Write-Host ("  Error Rate: {0}%"       -f $errPct) -ForegroundColor $errCol
    Write-Host ("  Avg RPS   : {0}"        -f $rps)
    Write-Host ""

    Stop-Simulation
}

