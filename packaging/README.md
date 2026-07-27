# DJOneHub for macOS

适用于 Apple Silicon 和 Intel Mac，支持大疆一代 4G 模块（USB `2ca3:4006`）及刷入移远 EC25 固件的模块（USB `2c7c:0125`）。

## 安装（推荐）

1. 将下载的 ZIP 完整解压，不要只单独拖出其中某个文件。
2. 打开“终端”，输入 `cd `（后面留一个空格），把解压得到的文件夹拖入终端，然后按回车。
3. 执行一次安装命令：

   ```sh
   ./install
   ```

   安装过程可能要求输入 Mac 管理员密码。输入密码时终端不会显示圆点或星号，这是正常现象。

安装完成后，可以在终端的任意目录执行：

```sh
djonehub start
```

程序会自动打开 `http://127.0.0.1:7575`。启动程序的终端需要保持打开；按 `Control+C` 即可停止。

也可以在另一个终端的任意目录执行：

```sh
djonehub stop
```

## 免安装使用

如果不想安装，也可以一直保留解压后的文件夹，在该目录执行：

   ```sh
   ./djonehub start
   ```

## 常用命令

```sh
djonehub status       # 查看状态
djonehub logs         # 查看实时日志
djonehub open         # 重新打开管理网页
djonehub start --demo # 不连接硬件，打开演示界面
```

## macOS 阻止打开时

本软件目前没有 Apple Developer ID 签名。首次启动如果被 macOS 阻止，请打开“系统设置 → 隐私与安全性”，确认仍要打开。

如果系统仍提示文件损坏，可在当前目录执行：

```sh
xattr -dr com.apple.quarantine ./djonehub ./bin ./lib
./djonehub start
```

请只对从可信发布页面下载并核对过 SHA-256 的文件执行该命令。

## 日志

程序日志保存在：

```text
~/Library/Logs/DJOneHub/djonehub.log
```

终端只显示启动、停止和错误摘要，不会持续刷出底层 USB 日志。

## 当前限制

- 支持 macOS 13 Ventura 至 macOS 26 Tahoe；发行包仅提供 Apple Silicon 版本，Intel Mac 需从源码构建。
- 仅监听本机地址，局域网中的其他设备无法直接访问管理网页。
- 本项目为非官方工具，与 DJI、Quectel、运营商及 eSIM 卡片厂商无隶属或授权关系。
- 使用短信、蜂窝数据和 eSIM 前，请确认运营商资费、漫游规则及当地法律要求。
