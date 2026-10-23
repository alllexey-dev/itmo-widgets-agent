# ITMO.Widgets agent installer for Windows, for the current user, no admin:
#   irm https://raw.githubusercontent.com/alllexey-dev/itmo-widgets-agent/main/install.ps1 | iex
# In a console with no parameters it shows the state and a menu; otherwise it installs and starts the task.
# Parameters: & ([scriptblock]::Create((irm https://raw.githubusercontent.com/alllexey-dev/itmo-widgets-agent/main/install.ps1))) -Uninstall -Yes
#   -Install, -Pair, -Service on|off|restart, -Status, -Unpair, -Uninstall, -NoService, -Version vX.Y.Z (or IW_VERSION), -Yes.
# A re-run of -Install upgrades in place.
param(
    [switch]$Install, [switch]$Pair, [ValidateSet('on', 'off', 'restart')][string]$Service, [switch]$Status,
    [switch]$Unpair, [switch]$Uninstall, [switch]$NoService, [string]$Version = $env:IW_VERSION, [switch]$Yes
)
# Under irm | iex this runs in the caller's scope: the preferences and the console encoding go back at the end.
$savedPreferences = $ErrorActionPreference, $ProgressPreference, [Console]::OutputEncoding
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # the progress bar slows Invoke-WebRequest down many times
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Repo = 'alllexey-dev/itmo-widgets-agent'
$Name = 'itmo-widgets-agent'
$Dir = Join-Path $env:LOCALAPPDATA $Name
$Exe = Join-Path $Dir "$Name.exe"
$Log = Join-Path $Dir 'agent.log'
$Task = 'ITMO.Widgets agent'
# Test-only: a local server with the release assets instead of GitHub.
$ReleaseBase = if ($env:IW_RELEASE_BASE) { $env:IW_RELEASE_BASE } else { "https://github.com/$Repo/releases/download" }
# Test-only: IW_TTY is a file with the answers instead of the console.
$Interactive = $env:IW_TTY -or ([Environment]::UserInteractive -and -not [Console]::IsInputRedirected)
$Mods = $NoService -or $Yes -or $PSBoundParameters.ContainsKey('Version')

function Get-Latest {
    if ($script:Version) { return $script:Version }
    try {
        $r = Invoke-RestMethod -Headers @{ Accept = 'application/vnd.github+json' } "https://api.github.com/repos/$Repo/releases?per_page=1"
    } catch {
        if ("$_" -match 'rate limit') { throw 'GitHub API: лимит запросов без токена исчерпан (60 в час на IP). Повторите через час или укажите версию: $env:IW_VERSION = "v0.1.0"' }
        throw "GitHub API не ответил ($_). Укажите версию: `$env:IW_VERSION = `"v0.1.0`""
    }
    # The newest release, prereleases included; /releases/latest skips them.
    if (-not @($r)[0].tag_name) { throw 'на GitHub нет ни одного релиза' }
    @($r)[0].tag_name
}

function Get-Installed { if (Test-Path $Exe) { (& $Exe version) } }
function Test-Paired { (Test-Path $Exe) -and [bool]((& $Exe status) -match '^Токен агента: есть') }

function Get-ServiceState {
    $t = Get-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
    if (-not $t) { 'none' } elseif ($t.State -eq 'Running') { 'running' } else { 'stopped' }
}

function Confirm-Action([string]$Question) {
    if ($Yes) { return $true }
    if (-not $Interactive) { throw "$Question Без консоли добавьте -Yes" }
    (Read-Answer "$Question [y/N]") -match '^(y|yes|д|да)$'
}

# Confirm-Default is a question that defaults to yes; it needs a console.
function Confirm-Default([string]$Question) { (Read-Answer "$Question [Y/n]") -notmatch '^(n|no|н|нет)$' }

function Read-Answer([string]$Question) {
    if ($env:IW_TTY) { Write-Host $Question; return Get-Content $env:IW_TTY -TotalCount 1 }
    Read-Host $Question
}

function Stop-Agent {
    Stop-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
    Get-Process $Name -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $Exe } | Stop-Process -Force
}

function Install-Binary {
    $v = Get-Latest
    $asset = "$Name-windows-amd64.exe"
    Write-Host "Скачиваем $asset $v"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
    New-Item -ItemType Directory $tmp | Out-Null
    try {
        Invoke-WebRequest -UseBasicParsing "$ReleaseBase/$v/$asset" -OutFile "$tmp\$asset"
        Invoke-WebRequest -UseBasicParsing "$ReleaseBase/$v/SHA256SUMS" -OutFile "$tmp\SHA256SUMS"
        $want = Get-Content "$tmp\SHA256SUMS" | ForEach-Object { $f = -split $_; if ($f[1] -eq $asset -or $f[1] -eq "*$asset") { $f[0] } }
        $got = (Get-FileHash -Algorithm SHA256 "$tmp\$asset").Hash
        if (-not $want -or $want -ne $got) { throw "контрольная сумма $asset не совпала с SHA256SUMS, файл не установлен" }
        New-Item -ItemType Directory -Force $Dir | Out-Null
        # A running exe is locked; the task starts again below.
        Stop-Agent
        Move-Item -Force "$tmp\$asset" $Exe
    } finally { Remove-Item -Recurse -Force $tmp }
    Write-Host "Установлен $Exe ($(Get-Installed))" -ForegroundColor Green
}

function Invoke-Pair {
    if (-not (Test-Path $Exe)) { throw 'агент не установлен' }
    if (-not $Interactive) { Write-Host "Нет консоли для подключения. Выполните потом: & `"$Exe`" pair" -ForegroundColor Yellow; return }
    & $Exe pair
    if ($LASTEXITCODE) { Write-Host "Подключение не закончено. Повторите: & `"$Exe`" pair" -ForegroundColor Yellow }
}

# Enable-Service (re)registers the logon task and starts it, so it also serves as restart.
function Enable-Service {
    if (-not (Test-Path $Exe)) { throw 'агент не установлен' }
    Stop-Agent
    $me = "$env:USERDOMAIN\$env:USERNAME"
    # conhost --headless runs the console exe without a window; cmd appends its log lines to the file.
    $action = New-ScheduledTaskAction -Execute "$env:WINDIR\System32\conhost.exe" `
        -Argument "--headless cmd.exe /d /c `"`"$Exe`" run --ui=plain 2>>`"$Log`"`""
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -Hidden `
        -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew
    Register-ScheduledTask -TaskName $Task -Action $action -Settings $settings -Force `
        -Trigger (New-ScheduledTaskTrigger -AtLogOn -User $me) `
        -Principal (New-ScheduledTaskPrincipal -UserId $me -LogonType Interactive -RunLevel Limited) | Out-Null
    Start-ScheduledTask -TaskName $Task
    Write-Host "Задача «$Task» запущена и стартует при входе в Windows." -ForegroundColor Green
    Write-Host "Логи: Get-Content -Wait `"$Log`""
}

function Disable-Service {
    Stop-Agent
    Unregister-ScheduledTask -TaskName $Task -Confirm:$false -ErrorAction SilentlyContinue
    Write-Host 'Служба выключена, автозапуска нет.'
}

function Invoke-Install {
    $fresh = -not (Test-Path $Exe)
    $was = Get-ServiceState
    Install-Binary
    if (Test-Paired) { Write-Host 'Телефон уже подключён, pair пропущен.' } else { Invoke-Pair }
    # An existing task is restarted without asking. Otherwise a console asks; without one a fresh install gets the
    # task and an upgrade keeps the user's choice.
    if ($NoService) { Write-Host "Служба не установлена (-NoService). Запуск вручную: & `"$Exe`" run" }
    elseif ($was -ne 'none') { Enable-Service }
    elseif ($Interactive -and -not $Yes) {
        if (Confirm-Default 'Запускать агента в фоне как службу (автозапуск)?') { Enable-Service }
        else { Write-Host 'Служба не создана. Включить потом: этот скрипт, пункт «Включить службу».' }
    } elseif ($fresh -or $Yes) { Enable-Service }
}

function Show-State {
    $v = Get-Installed
    $l = try { Get-Latest } catch { 'неизвестна' }
    if (-not $v) { Write-Host "Агент:   не установлен (последняя версия $l)" -ForegroundColor Yellow; return }
    $note = if ("v$($v -replace '^v')" -eq $l) { 'последняя' } else { "есть $l" }
    Write-Host "Агент:   $v ($note), $Exe"
    $agentName = if ($env:IW_AGENT_NAME) { $env:IW_AGENT_NAME } else { $env:COMPUTERNAME }
    if (Test-Paired) { Write-Host "Телефон: подключён, имя $agentName" -ForegroundColor Green }
    else { Write-Host 'Телефон: не подключён' -ForegroundColor Yellow }
    switch (Get-ServiceState) {
        'running' { Write-Host 'Служба:  работает' -ForegroundColor Green }
        'stopped' { Write-Host 'Служба:  остановлена' -ForegroundColor Red }
        default { Write-Host 'Служба:  не установлена' }
    }
}

function Show-Status {
    Show-State
    if (-not (Test-Path $Exe)) { return }
    if ((Get-ServiceState) -eq 'running') { Write-Host "Экран службы вживую: & `"$Exe`" attach (q - закрыть, служба продолжит работу)" }
    Write-Host ''
    & $Exe status
    if (Test-Path $Log) { Write-Host "`nПоследние строки лога:"; Get-Content $Log -Tail 30 }
}

function Invoke-Unpair {
    if (-not (Test-Path $Exe)) { throw 'агент не установлен' }
    if (-not (Confirm-Action 'Отключить агента от аккаунта? Токен и сессия ИТМО будут удалены с этого компьютера.')) { return }
    & $Exe unpair
    if ($LASTEXITCODE) { throw 'unpair не удался: удалите агент в приложении, Профиль -> «Устройства»' }
}

function Invoke-Uninstall {
    if (-not (Confirm-Action 'Удалить агента полностью: службу, программу, токен и сессию ИТМО?')) { return }
    Disable-Service
    $stateDir = $null
    if (Test-Path $Exe) {
        $stateDir = (& $Exe status) -match '^Папка: ' -replace '^Папка: '
        & $Exe unpair
        if ($LASTEXITCODE) { Write-Host 'unpair не удался: удалите агент в приложении, Профиль -> «Устройства»' -ForegroundColor Yellow }
    }
    Remove-Item -Recurse -Force $Dir -ErrorAction SilentlyContinue
    Write-Host 'Агент удалён: программа, служба, токен и сессия ИТМО.' -ForegroundColor Green
    if ($stateDir -and (Test-Path $stateDir)) { Write-Host "В $stateDir остались файлы без секретов (state.json, agent.env); удалите папку, если не нужна." }
}

function Show-Menu {
    # One GitHub API call per run: the unauthenticated limit is 60 an hour.
    if (-not $script:Version) { $script:Version = try { Get-Latest } catch { $null } }
    while ($true) {
        Write-Host "`nITMO.Widgets agent"
        Show-State
        Write-Host ''
        $items = [ordered]@{ '1' = 'Установить или обновить' }
        if (Test-Path $Exe) {
            $s = Get-ServiceState
            $paired = Test-Paired
            if (-not $paired) { $items['2'] = 'Подключить к телефону (pair)' }
            if ($s -eq 'none') { $items['3'] = 'Включить службу (автозапуск)' }
            if ($s -ne 'none') { $items['4'] = 'Выключить службу'; $items['5'] = 'Перезапустить службу' }
            $items['6'] = 'Статус и логи'
            # attach draws a screen: it needs the console itself, not redirected input or output.
            if ($s -eq 'running' -and -not $env:IW_TTY -and -not [Console]::IsInputRedirected -and -not [Console]::IsOutputRedirected) {
                $items['7'] = 'Открыть экран службы (attach)'
            }
            if ($paired) { $items['8'] = 'Отключить агента (unpair)' }
        }
        if ((Test-Path $Exe) -or (Get-ServiceState) -ne 'none') { $items['9'] = 'Удалить полностью' }
        $items['0'] = 'Выход'
        $items.GetEnumerator() | ForEach-Object { Write-Host "$($_.Key). $($_.Value)" }
        $choice = (Read-Host '>').Trim()
        if (-not $items.Contains($choice)) { Write-Host 'Нет такого пункта.'; continue }
        try {
            switch ($choice) {
                '0' { return }
                '1' { Invoke-Install }
                '2' { Invoke-Pair }
                '3' { Enable-Service }
                '4' { Disable-Service }
                '5' { Enable-Service }
                '6' { Show-Status }
                '7' { & $Exe attach; if ($LASTEXITCODE) { Write-Host "Экран не открылся: & `"$Exe`" attach" -ForegroundColor Yellow } }
                '8' { Invoke-Unpair }
                '9' { Invoke-Uninstall }
            }
        } catch { Write-Host "Ошибка: $_" -ForegroundColor Red }
    }
}

# The agent writes UTF-8.
[Console]::OutputEncoding = [Text.Encoding]::UTF8
try {
    if ($Pair) { Invoke-Pair }
    elseif ($Service -eq 'off') { Disable-Service }
    elseif ($Service) { Enable-Service }
    elseif ($Status) { Show-Status }
    elseif ($Unpair) { Invoke-Unpair }
    elseif ($Uninstall) { Invoke-Uninstall }
    elseif ($Install -or $Mods -or -not $Interactive) { Invoke-Install }
    else { Show-Menu }
} finally { $ErrorActionPreference, $ProgressPreference, [Console]::OutputEncoding = $savedPreferences }
