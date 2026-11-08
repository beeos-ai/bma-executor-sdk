<#
.SYNOPSIS
  Installs the BMA Windows executor and registers it to start at logon.
.DESCRIPTION
  Run as Administrator on the Windows machine. The executor must run in the
  interactive session of the (auto-logged-on) user, so it is started by a
  scheduled task at that user's logon, not by a service. Optionally installs a
  pinned MIT build of Cua Driver and starts its daemon the same way (computer use).
  The machine token is read from -MachineTokenFile (preferred) or -MachineToken,
  written only to <InstallDir>\executor.env (readable by SYSTEM, Administrators
  and the task user) and never printed.
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$BaseUrl,
  [string]$MachineTokenFile,
  [string]$MachineToken,
  [string]$MachineId,
  [string]$InstallDir = 'C:\bma',
  [string]$User = "$env:USERDOMAIN\$env:USERNAME",
  [string]$CuaDriverDir,
  [string]$CuaDriverSha256,
  [switch]$SkipTask,
  [switch]$StartNow
)
$ErrorActionPreference = 'Stop'

if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw 'Run this script as Administrator.'
}
if ($MachineTokenFile) { $MachineToken = (Get-Content -LiteralPath $MachineTokenFile -Raw).Trim() }
if (-not $MachineToken) { throw 'Give -MachineTokenFile (or -MachineToken).' }
if ($BaseUrl -notmatch '^https?://') { throw '-BaseUrl must be the deployment origin, e.g. https://executor.<env>.bma.beeos.ai' }

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$bin = Join-Path $InstallDir 'bin'
$logs = Join-Path $InstallDir 'logs'
foreach ($dir in $InstallDir, $bin, $logs, (Join-Path $InstallDir 'work'), (Join-Path $InstallDir 'files')) {
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
}
$exe = Join-Path $bin 'windows-hand.exe'
Get-ScheduledTask -TaskName 'BMA Executor' -ErrorAction SilentlyContinue | Stop-ScheduledTask -ErrorAction SilentlyContinue
Get-Process -Name 'windows-hand' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe } | Stop-Process -Force
Copy-Item -LiteralPath (Join-Path $here 'windows-hand.exe') -Destination $exe -Force

# Cua Driver (MIT build only; the AGPL perception extension and the FSL
# space daemon are not installed). The checksum is mandatory.
$cua = $null
if ($CuaDriverDir) {
  if (-not $CuaDriverSha256) { throw '-CuaDriverSha256 is required with -CuaDriverDir.' }
  $source = Join-Path $CuaDriverDir 'cua-driver.exe'
  $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $source).Hash
  if ($actual -ne $CuaDriverSha256.ToUpperInvariant().Replace('SHA256:', '')) { throw "cua-driver.exe sha256 mismatch: $actual" }
  $cuaDir = Join-Path $InstallDir 'cua-driver'
  New-Item -ItemType Directory -Force -Path $cuaDir | Out-Null
  Copy-Item -Path (Join-Path $CuaDriverDir '*') -Destination $cuaDir -Recurse -Force
  $cua = Join-Path $cuaDir 'cua-driver.exe'
}

$envFile = Join-Path $InstallDir 'executor.env'
$lines = @("BMA_BASE_URL=$BaseUrl", "BMA_MACHINE_TOKEN=$MachineToken")
if ($MachineId) { $lines += "BMA_MACHINE_ID=$MachineId" }
if ($cua) { $lines += "BMA_CUA_DRIVER=$cua" }
Set-Content -LiteralPath $envFile -Value $lines -Encoding ascii
# Only SYSTEM, Administrators and the task user may read the token.
& icacls.exe $envFile /inheritance:r /grant:r 'SYSTEM:(F)' 'BUILTIN\Administrators:(F)' "${User}:(R)" | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'icacls failed on executor.env' }

if (-not $SkipTask) {
  function Register-LogonTask($name, $command, $delay) {
    $action = New-ScheduledTaskAction -Execute 'cmd.exe' -Argument "/d /s /c `"$command`""
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $User
    $trigger.Delay = $delay
    $principal = New-ScheduledTaskPrincipal -UserId $User -LogonType Interactive -RunLevel Limited
    $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 `
      -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
    Register-ScheduledTask -TaskName $name -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null
  }
  if ($cua) {
    # The driver daemon owns the desktop; the executor talks to it over \\.\pipe\cua-driver.
    Register-LogonTask 'BMA Cua Driver' "set CUA_TELEMETRY=0&& `"$cua`" serve >> `"$logs\cua-driver.log`" 2>&1" 'PT2S'
  } else {
    Unregister-ScheduledTask -TaskName 'BMA Cua Driver' -Confirm:$false -ErrorAction SilentlyContinue
  }
  Register-LogonTask 'BMA Executor' "`"$exe`" -env-file `"$envFile`" >> `"$logs\executor.log`" 2>&1" 'PT15S'
  if ($StartNow) {
    if ($cua) { Start-ScheduledTask -TaskName 'BMA Cua Driver' }
    Start-ScheduledTask -TaskName 'BMA Executor'
  }
}
Write-Host "Installed to $InstallDir (user $User). Logs: $logs. The executor starts at that user's next logon" $(if ($StartNow) { 'and now.' } else { '(or: Start-ScheduledTask -TaskName "BMA Executor").' })
