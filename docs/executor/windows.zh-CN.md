# Windows 上的 Go 执行端

Go 执行内核（`NewCore`、`NewProcessCapability`、`NewSessionFiles`）在 Windows 上的行为，以及如何在机器上运行。可安装构建是 `examples/windows-hand` + `packaging/windows`（CI 产物 `bma-windows-hand`）。完整英文表格见 [windows.md](windows.md)，要点：

- **环境变量**：除 `PATH/HOME/USER/LANG/TMPDIR` 外继承 `SystemRoot`、`windir`、`ComSpec`、`PATHEXT`、`TEMP/TMP`、`USERPROFILE`、`APPDATA`、`LOCALAPPDATA`、`ProgramFiles*` 等；名字大小写不敏感合并；`DenyEnv` 仍生效。
- **命令行**：`cmd.exe /d /s /c <命令>` 用 `SysProcAttr.CmdLine` 原样传递（缺 `/s` 时补上），PowerShell 的 `"` 与 `$` 不被改写。
- **进程树**：每个命令/进程/MCP 在 Job Object（`KILL_ON_JOB_CLOSE`）中，先挂起、入 Job、再恢复；取消、超时、`process.terminate`、会话结束都终止整棵树（含进程退出后遗留的子进程）。
- **中断**：`process.interrupt` 向进程组发 `CTRL_BREAK`（执行端无控制台时自建隐藏控制台）；`process.terminate` 先同样请求，`grace_ms` 后杀整树。`exit_signal` 为 `SIGINT/SIGTERM/SIGKILL`。
- **PTY**：`io_mode: pty` 用 ConPTY（Windows 10 1809+），中断写入 `^C`，输出为合并流，终端 24×80。内核不注册 `terminal.*`，由宿主基于 `io_mode: pty` 实现终端会话。
- **cwd**：绝对 cwd 等于工作根（如机器配置的 `C:\bma\work`）时即会话工作区；其它绝对目录必须在工作区内；按真实路径比较（符号链接、8.3 短名、大小写）。
- **会话文件/文件传输**：挂载文件带只读属性，变更时替换只读旧文件，取消挂载则删除；`NewFileTransferer()` 提供 `process.start file_transfer`（原子替换并保留目标的只读属性）。
- **computer use**：`CoreOptions.ComputerUse`（`NewCuaDriver`）经 Cua Driver 守护进程提供 `computer_observe`/`computer_action`；仅用 MIT 部分，遥测关闭；无可用驱动则不声明 `computer_use`。
- **会话模型**：执行端与 Cua 守护进程都须在自动登录用户的交互会话（Session 1）里，以登录触发的计划任务启动，不能是服务。
- **安装**：见 zip 内 `README.txt`；`install.ps1` 注册计划任务 `BMA Executor`（可选 `BMA Cua Driver`），`executor.env`（含令牌）只允许 SYSTEM、Administrators 与该用户读取，令牌不打印。
