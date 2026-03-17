# OtelContext V5.4 Development Startup Script
# Starts: Backend (Air) + Frontend (Vite)
# Chaos test services are started separately via test/run_simulation.ps1

$ErrorActionPreference = "Stop"
$PidFile = Join-Path $PSScriptRoot ".dev-pids"

Write-Host ""
Write-Host "================================" -ForegroundColor Cyan
Write-Host " OtelContext V5.4 (DEV MODE)" -ForegroundColor Cyan
Write-Host " Starting Development Stack..." -ForegroundColor Cyan
Write-Host "================================" -ForegroundColor Cyan
Write-Host ""

# ── Kill previous session ──
Write-Host "Cleaning up old processes..." -ForegroundColor Yellow

if (Test-Path $PidFile) {
    $oldPids = Get-Content $PidFile
    foreach ($procId in $oldPids) {
        try {
            $proc = Get-Process -Id $procId -ErrorAction SilentlyContinue
            if ($proc) {
                Get-CimInstance Win32_Process | Where-Object { $_.ParentProcessId -eq $procId } | ForEach-Object {
                    Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
                }
                Stop-Process -Id $procId -Force -ErrorAction SilentlyContinue
            }
        }
        catch { }
    }
    Remove-Item $PidFile -Force
}

Get-Process -Name "OtelContext" -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

# ── Start services ──
$pids = @()

# 1. OtelContext Backend (UI is embedded Go templates — no separate frontend process needed)
Write-Host "[1/1] Starting OtelContext Backend..." -ForegroundColor Green
$p = Start-Process pwsh -ArgumentList "-NoExit", "-Command", "Set-Location '$PSScriptRoot'; air" -PassThru
$pids += $p.Id

# Save PIDs
$pids | Out-File $PidFile -Force

Write-Host ""
Write-Host "================================" -ForegroundColor Green
Write-Host " Dev Stack Started!" -ForegroundColor Green
Write-Host "================================" -ForegroundColor Green
Write-Host ""
Write-Host "  OtelContext UI:           http://localhost:8080" -ForegroundColor White
Write-Host "  OtelContext gRPC:         localhost:4317" -ForegroundColor White
Write-Host "  Prometheus Metrics: http://localhost:8080/metrics/prometheus" -ForegroundColor White
Write-Host "  Health Check:       http://localhost:8080/api/health" -ForegroundColor White
Write-Host "  MCP Endpoint:       http://localhost:8080/mcp" -ForegroundColor White
Write-Host ""
Write-Host "  To run chaos tests: .\test\run_simulation.ps1" -ForegroundColor DarkGray
Write-Host "  Re-run this script to restart cleanly." -ForegroundColor DarkGray
Write-Host ""

