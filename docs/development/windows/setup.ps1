# ZBBS Windows Development Setup
# Run as: powershell -ExecutionPolicy Bypass -File setup.ps1

$ErrorActionPreference = "Stop"

Write-Host ""
Write-Host "=== ZBBS Windows Development Setup ===" -ForegroundColor Cyan
Write-Host ""

# --- .wslconfig ---

$wslConfigPath = "$env:USERPROFILE\.wslconfig"
$wslConfigContent = @"
[general]
instanceIdleTimeout=-1

[wsl2]
networkingMode=mirrored
vmIdleTimeout=-1
"@

$needsWslRestart = $false

if (Test-Path $wslConfigPath) {
    $existing = Get-Content $wslConfigPath -Raw
    $hasInstanceTimeout = $existing -match "instanceIdleTimeout"
    $hasMirrored = $existing -match "networkingMode\s*=\s*mirrored"
    $hasVmTimeout = $existing -match "vmIdleTimeout"

    if ($hasInstanceTimeout -and $hasMirrored -and $hasVmTimeout) {
        Write-Host "[OK] .wslconfig already configured" -ForegroundColor Green
    }
    else {
        Write-Host "[!!] .wslconfig exists but is missing required settings" -ForegroundColor Yellow
        Write-Host "     Expected contents:" -ForegroundColor Yellow
        Write-Host ""
        Write-Host $wslConfigContent -ForegroundColor Gray
        Write-Host ""
        Write-Host "     Current file: $wslConfigPath" -ForegroundColor Yellow
        Write-Host "     Please update it manually, or delete it and re-run this script." -ForegroundColor Yellow
    }
}
else {
    Set-Content -Path $wslConfigPath -Value $wslConfigContent -Encoding UTF8
    Write-Host "[OK] Created .wslconfig at $wslConfigPath" -ForegroundColor Green
    $needsWslRestart = $true
}

# --- Hosts file ---

$hostsPath = "C:\Windows\System32\drivers\etc\hosts"
$hostsEntry = "127.0.0.1 zbbs.local"

$hostsContent = Get-Content $hostsPath -Raw -ErrorAction SilentlyContinue
if ($hostsContent -match "zbbs\.local") {
    Write-Host "[OK] Hosts file already has zbbs.local entry" -ForegroundColor Green
}
else {
    $isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)

    if ($isAdmin) {
        Add-Content -Path $hostsPath -Value "`n$hostsEntry"
        Write-Host "[OK] Added zbbs.local to hosts file" -ForegroundColor Green
    }
    else {
        Write-Host "[!!] Hosts file missing zbbs.local entry" -ForegroundColor Yellow
        Write-Host "     Add this line to $hostsPath" -ForegroundColor Yellow
        Write-Host "     (requires running Notepad as Administrator):" -ForegroundColor Yellow
        Write-Host ""
        Write-Host "     $hostsEntry" -ForegroundColor White
        Write-Host ""
    }
}

# --- WSL restart if needed ---

if ($needsWslRestart) {
    Write-Host ""
    Write-Host "Restarting WSL to apply .wslconfig changes..." -ForegroundColor Cyan
    wsl --shutdown
    Start-Sleep -Seconds 2
}

# --- Start WSL services ---

Write-Host ""
Write-Host "Starting WSL services..." -ForegroundColor Cyan

wsl -u root bash -c "systemctl start postgresql && systemctl start php8.3-fpm && systemctl start apache2 && systemctl start mercure"

if ($LASTEXITCODE -eq 0) {
    Write-Host "[OK] All services started" -ForegroundColor Green
}
else {
    Write-Host "[!!] Failed to start services. Run the setup playbook:" -ForegroundColor Red
    Write-Host '     wsl -u root bash -c "cd /mnt/c/dev/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/setup.yml"' -ForegroundColor Yellow
    exit 1
}

# --- Verify ---

Write-Host ""
Write-Host "Verifying..." -ForegroundColor Cyan

$uptime = wsl -u root bash -c "uptime -p"
Write-Host "  WSL uptime: $uptime" -ForegroundColor Gray

$services = @("apache2", "php8.3-fpm", "postgresql", "mercure")
foreach ($svc in $services) {
    $status = wsl -u root bash -c "systemctl is-active $svc 2>/dev/null"
    if ($status -eq "active") {
        Write-Host "  [OK] $svc" -ForegroundColor Green
    }
    else {
        Write-Host "  [!!] $svc is $status" -ForegroundColor Red
    }
}

Write-Host ""
Write-Host "Site should be available at: http://zbbs.local/" -ForegroundColor Cyan
Write-Host ""
