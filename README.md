# GoPit

## 用途

GoPit 用于在服务端与内网客户端之间建立隧道，支持服务端统一管理 HTTP、TCP、UDP 连接，以及客户端多隧道连接、状态查看和后台运行。

## 安装说明

macOS、Linux 和 Windows 的 `amd64`、`arm64` 可执行文件会发布到 GitHub Release。安装脚本会自动判断当前系统和架构：

```bash
curl -fsSL https://raw.githubusercontent.com/newpanjing/gopit/main/install.sh | bash
```

安装后重新打开终端即可使用 `pit`。升级当前版本：

```bash
pit upgrade
```

也可以直接运行本地脚本：

```bash
./install.sh
./install.sh upgrade
```

## 简介

```bash
pit start                 # 后台启动服务端模式
pit join <token> -s host  # 后台启动客户端模式
pit tui                   # 打开当前模式界面
pit stop                  # 停止当前模式
pit restart               # 重启当前模式
pit logs                  # 查看当前模式日志
```

`start` 和 `join` 会将当前模式保存到 `pit.yaml`。服务端 TUI 与客户端 TUI 独立显示；TUI 中按 `m` 可选择下次启动的模式。

## 服务端配置

服务端配置默认为 `server.yaml`，可从示例复制：

```bash
cp configs/server.example.yaml server.yaml
pit start
pit tui
```

主要配置：

```yaml
server:
  tunnel_listen: ":7001"
  http_listen: ":80"
  https_listen: ":443"
connections: []
```

服务端 TUI 可以创建和编辑 HTTP、TCP、UDP 隧道、Token 和目标地址。配置修改会持久化并即时通知在线客户端。

## 客户端配置

推荐直接使用 Token 加入：

```bash
pit join <token> -s 192.168.1.188
```

客户端配置默认为 `client.yaml`，支持保存多个隧道。客户端后台状态可通过以下命令查看：

```bash
pit tui
pit stop
```
