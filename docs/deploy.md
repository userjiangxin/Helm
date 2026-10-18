# 部署 Helm

Helm 由两部分组成：

- **控制面**：一个 Node 服务，包含 API、看板和数据库。全局只部署一份，放在一台常开的机器上。
- **helmd**：一个 Go 二进制，每台要干活的机器各装一份，只主动向控制面建立连接。

## 方式一：单机试用（5 分钟）

适合先在自己电脑上体验。数据库使用内嵌的 PGlite，不需要 Docker。

```bash
git clone https://github.com/userjiangxin/Helm.git && cd Helm
make demo                         # 需要 Go 1.24+、Node 22+、pnpm
```

打开 http://127.0.0.1:8080 ，进入“机器”页，点“添加机器”，把页面给出的命令在同一台电脑上执行一次。之后就可以新建目标和任务，派给本机的 Claude Code。

## 方式二：长期使用（Tailscale + Docker Compose）

### 1. 准备控制面机器

选一台常开的机器（云主机或家里的服务器），安装 Docker 和 Tailscale，并加入你的 tailnet。

```bash
git clone https://github.com/userjiangxin/Helm.git && cd Helm
echo "HELM_ALLOWED_USERS=你的Tailscale登录邮箱" > .env
docker compose up -d --build
tailscale serve --bg 8080         # 以 https://<机器名>.<tailnet>.ts.net 对 tailnet 内提供服务
```

`tailscale serve` 会在请求里注入访问者的 Tailscale 身份，控制面据此只放行 `HELM_ALLOWED_USERS` 里列出的账号。如果有设备不在 tailnet 里，可以额外设置 `HELM_BOARD_PASSWORD`，用户名固定为 `helm`。

如果既没有设置 `HELM_ALLOWED_USERS` 也没有设置 `HELM_BOARD_PASSWORD`，而监听地址又不是 127.0.0.1，控制面会拒绝启动，避免看板被无保护地暴露出去。

### 2. 接入每台干活的机器

在每台机器上（本机、云服务器、GPU 节点）：

1. 安装 Tailscale，加入同一个 tailnet。
2. 安装并登录要用的 agent CLI：`claude`（Claude Code），以及可选的 `codex`。
3. 在看板“机器”页点“添加机器”，复制命令执行：

```bash
git clone https://github.com/userjiangxin/Helm.git && cd Helm
deploy/install.sh --server https://<控制面机器名>.<tailnet>.ts.net --code <注册码> --labels gpu
```

脚本会从源码编译 helmd（需要 Go），把机器 token 写入 `~/.helm/credentials`，然后以用户服务的方式启动：Linux 上是 systemd，macOS 上是 launchd。

- 查看日志：Linux 用 `journalctl --user -u helmd -f`，macOS 用 `tail -f ~/.helm/helmd.log`
- GPU 机器上一定要执行 `sudo loginctl enable-linger $USER`，否则你退出 SSH 后 helmd 会被停掉
- 标签用于派活：任务的“机器标签”填 `gpu`，就只会派给带 `gpu` 标签的机器

### 3. 升级

```bash
git pull
docker compose up -d --build                    # 控制面；数据库迁移在启动时自动执行
deploy/install.sh --server … --code <新注册码>    # helmd；也可以只重新编译并重启服务
```

## 安全边界

- helmd 不监听任何端口，只主动连接控制面
- 机器 token 只保存在 `~/.helm/credentials`（权限 0600），不会出现在进程参数里
- 每次工具调用都要先经过 helmd 的策略引擎；helmd 不在线时，Claude Code 的工具调用一律被拒绝
- 控制面与 helmd 不保存任何模型 API key：每台机器上的 Claude Code、Codex 各自登录
