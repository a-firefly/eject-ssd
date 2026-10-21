# macOS 外接 SSD 弹出工具

Go 实现，无第三方库。运行时使用 macOS 自带的 `diskutil`、`plutil` 和 `lsof`。

在项目目录编译：

```sh
cd /Users/dev/codes/o/eject-ssd
go build -o /tmp/eject-ssd .
```

运行：

```sh
/tmp/eject-ssd                  # 自动选择唯一的外接 SSD，安全弹出
/tmp/eject-ssd --list           # 列出外接物理磁盘
/tmp/eject-ssd --dry-run        # 预览，不弹出
/tmp/eject-ssd --diagnose       # 仅检查占用进程，不弹出
/tmp/eject-ssd --disk disk4     # 多块磁盘时，指定整块外接磁盘
/tmp/eject-ssd --force          # 强制卸载，然后弹出
```

默认调用 macOS `diskutil eject`，由系统卸载整个磁盘的宗卷并弹出。失败时输出系统错误和 `lsof` 占用信息，包括物理盘对应的 APFS 宗卷。

如果目标盘的 `/Volumes/ssd` 宗卷被 `adb` 占用，且 `lsof` 的 `txt` 记录确认可执行文件为 `/Volumes/ssd/sdk/Android/platform-tools/adb`，工具会自动执行该路径的 `adb kill-server`，重新确认磁盘身份后重试失败的弹出步骤。每次运行最多停止一次 adb；命令失败或重试失败就停止并报告错误。停止 adb 会断开当前 adb 调试连接。

`--diagnose` 和 `--dry-run` 不会停止 adb。其他路径的 adb 和其他应用不会被自动关闭，也不会自动强制卸载。其他占用需先保存文件、停止复制/备份、关闭占用应用，并让其他终端执行 `cd ~`，然后重试。

`--force` 调用 `diskutil unmountDisk force` 后再执行弹出，可能导致未保存的数据丢失；仅在确认已停止写入后使用。强制卸载也可能失败，只有看到“已成功弹出，可以拔出 SSD”才拔线。若卸载成功但弹出失败，磁盘可能仍连接着但已不再挂载，可用“磁盘工具”检查。

多个 SSD 时必须明确指定 `--disk`；不会选择内置磁盘、虚拟磁盘或分区。某些 USB 桥接器不报告 SSD 类型，可先 `--list` 确认后使用 `--disk`。磁盘编号会随重连变化，不要在快捷命令中固定编号。

普通权限下 `lsof` 可能看不到其他用户或系统进程；需要时只提升诊断权限：

```sh
sudo /tmp/eject-ssd --diagnose
```

工具会把自身工作目录切换到 `/`，但无法改变启动它的终端或其他进程的工作目录。诊断没有列出进程，也不代表磁盘一定没有占用。命令超时或被中断后，需重新检查磁盘状态。

`/tmp` 中的二进制可能被系统清理。需要长期使用时，可手动安装到个人命令目录：

```sh
mkdir -p "$HOME/.local/bin"
go build -o "$HOME/.local/bin/eject-ssd" .
```

将 `~/.local/bin` 加入 `PATH` 后即可直接运行 `eject-ssd`。

退出码：成功或只读检查完成为 `0`，弹出失败/识别失败为 `1`，参数错误为 `2`。测试：`go test .`；测试使用模拟磁盘，不会弹出真实设备。
