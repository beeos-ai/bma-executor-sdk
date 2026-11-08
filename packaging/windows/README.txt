BMA Windows executor (windows-hand)
===================================

Contents
  windows-hand.exe   machine executor: cmd.exe tool, files, MCP, processes (Job
                     Object trees, ConPTY, interrupts), file transfers, session
                     files, optional computer use through Cua Driver
  install.ps1        registers the executor to start at logon (run as Administrator)
  uninstall.ps1      removes it
  NOTICE-cua-driver.txt   license note for the optional Cua Driver
  SHA256SUMS         checksums of the files above

Install (Administrator PowerShell, on the machine; the target user is the
auto-logged-on interactive user):

  .\install.ps1 -BaseUrl https://executor.<env>.bma.beeos.ai `
                -MachineTokenFile C:\secrets\machine-token.txt -MachineId mch_... -StartNow

With computer use (Cua Driver v0.44.0, MIT build, checksum required):

  .\install.ps1 ... -CuaDriverDir C:\drop\cua-driver -CuaDriverSha256 <sha256 of cua-driver.exe>

Layout (default C:\bma): bin\, work\ (session workspaces), files\ (session
files), logs\ (executor.log, cua-driver.log), executor.env (token; ACL limited
to SYSTEM, Administrators, the task user). Two scheduled tasks run at logon of
that user in the interactive session: "BMA Cua Driver" (if installed, +2 s) and
"BMA Executor" (+15 s).

Check: Get-ScheduledTask 'BMA Executor'; Get-Content C:\bma\logs\executor.log -Tail 20
  (a line "registered as ..., ready ...s after start" means it is online).
Remove: .\uninstall.ps1 [-Purge]
