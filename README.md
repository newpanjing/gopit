# GoPit

`pit` 是一个支持服务端与客户端模式的隧道管理工具。执行 `pit start` 进入服务端模式，执行 `pit join` 进入客户端模式；当前模式会保存到 `pit.yaml`，后续 `tui`、`restart` 与开机启动会自动恢复该模式。

## 安装

发布版本支持 macOS（Intel、Apple Silicon）与 Linux（x86_64、arm64）。安装脚本会自动识别系统与架构，并下载对应的 GitHub Release 资产到 `~/.local/bin/pit`。

在源码目录内执行时，脚本会尝试从 Git remote 读取仓库地址：

```bash
./instal.sh install
```

通过 curl 下载脚本或当前目录没有 Git remote 时，指定 GitHub 仓库：

```bash
curl -fsSL https://raw.githubusercontent.com/OWNER/REPOSITORY/main/instal.sh | bash -s -- install --repo OWNER/REPOSITORY
```

也可使用环境变量：

```bash
GOPIT_REPOSITORY=OWNER/REPOSITORY ./instal.sh install
```

首次安装后，确保 `~/.local/bin` 在 `PATH` 中：

```bash
export PATH="$HOME/.local/bin:$PATH"
```

## 升级

升级会下载最新 Release 并替换本地二进制：

```bash
./instal.sh upgrade --repo OWNER/REPOSITORY
```

安装指定版本：

```bash
./instal.sh install --repo OWNER/REPOSITORY --version v1.0.0
```

## 运行

安装后可直接使用：

```bash
pit start
pit tui
pit join <token> -s <server>
```

也可以让脚本在本地不存在二进制时自动安装后运行：

```bash
./instal.sh run --repo OWNER/REPOSITORY start
```

## 开机启动

先通过 `start` 或 `join` 选择并保存运行模式，再开启自动恢复：

```bash
pit startup enable
pit startup disable
```

## 发布版本

推送形如 `v1.0.0` 的 Git 标签会触发 GitHub Actions，自动构建 macOS/Linux 多架构压缩包并创建 GitHub Release：

```bash
git tag v1.0.0
git push origin v1.0.0
```
