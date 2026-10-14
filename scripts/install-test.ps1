# Runs install.ps1 against a fake release: scripts/install-test.ps1 INSTALLER VERSION.
# IW_RELEASE_BASE serves VERSION/<assets> with a good SHA256SUMS and bad/<assets> with a wrong one.
# Pairing is skipped through a token fixture. Answers n to the service question, then registers and removes a real
# logon task.
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSReviewUnusedParameter', 'Installer', Justification = 'used in Invoke-Installer')]
param([string]$Installer, [string]$V)
$ErrorActionPreference = 'Stop'
$root = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
$env:LOCALAPPDATA = "$root\local"
$env:IW_STATE_DIR = "$root\state"
$env:IW_BACKEND_URL = 'http://127.0.0.1:9'
New-Item -ItemType Directory -Force $env:LOCALAPPDATA, $env:IW_STATE_DIR | Out-Null
Set-Content "$env:IW_STATE_DIR\agent-token" 'iwa_test_fixture'
$exe = "$env:LOCALAPPDATA\itmo-widgets-agent\itmo-widgets-agent.exe"
$task = 'ITMO.Widgets agent'
function Invoke-Installer { & $Installer @args *>&1 | Out-String | Tee-Object -Variable out | Write-Output; $out }
function Assert-True($ok, [string]$what) { if (-not $ok) { throw "FAIL: $what" } }

$out = Invoke-Installer -Install -NoService -Version $V
Assert-True ((& $exe version) -eq $V.TrimStart('v')) 'installed version'
Assert-True ($out -match 'pair пропущен') 'pairing not skipped with a token'

$out = Invoke-Installer -Status -Version $V
Assert-True ($out -match "Агент:   $($V.TrimStart('v')) \(последняя\)") 'status: version'
Assert-True ($out -match 'Телефон: подключён') 'status: paired'
Assert-True ($out -match 'Служба:  не установлена') 'status: service'

# A console answering "n" to the service question: no task.
Set-Content "$root\answers" 'n'
$env:IW_TTY = "$root\answers"
$out = Invoke-Installer -Install -Version $V
Remove-Item Env:IW_TTY
Assert-True ($out -match 'Запускать агента в фоне как службу') 'no service question'
Assert-True (-not (Get-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue)) 'the answer n left a task'

$before = (Get-FileHash $exe).Hash
try { Invoke-Installer -Install -NoService -Version bad | Out-Null; $installed = $true } catch { $installed = $false }
Assert-True (-not $installed) 'a wrong checksum installed'
Assert-True ((Get-FileHash $exe).Hash -eq $before) 'a wrong checksum replaced the binary'

Invoke-Installer -Service on -Version $V | Out-Null
Start-Sleep 5
Assert-True ((Get-ScheduledTask -TaskName $task).State -eq 'Running') 'the task is not running'
Assert-True (Get-Process itmo-widgets-agent | Where-Object { $_.Path -eq $exe }) 'the agent process is not running'

Invoke-Installer -Uninstall -Yes | Out-Null
Assert-True (-not (Get-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue)) 'uninstall left the task'
Assert-True (-not (Test-Path $exe)) 'uninstall left the binary'
Assert-True (-not (Test-Path "$env:IW_STATE_DIR\agent-token")) 'uninstall left the token'
Write-Output 'install.ps1: all checks passed'
