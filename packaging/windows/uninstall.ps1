<#
.SYNOPSIS
  Removes the BMA Windows executor tasks and binaries. -Purge also deletes the
  work root, session files, logs and executor.env (the machine token).
#>
[CmdletBinding()]
param([string]$InstallDir = 'C:\bma', [switch]$Purge)
$ErrorActionPreference = 'Stop'
foreach ($name in 'BMA Executor', 'BMA Cua Driver') {
  Stop-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue
  Unregister-ScheduledTask -TaskName $name -Confirm:$false -ErrorAction SilentlyContinue
}
foreach ($exe in 'windows-hand', 'cua-driver') {
  Get-Process -Name $exe -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$InstallDir\*" } | Stop-Process -Force
}
Remove-Item -Recurse -Force -ErrorAction SilentlyContinue (Join-Path $InstallDir 'bin'), (Join-Path $InstallDir 'cua-driver')
if ($Purge) { Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $InstallDir }
Write-Host "Removed the BMA executor from $InstallDir."
