# ServerStatus

轻量、自托管的 VPS 探针。服务端（Server）用网页集中展示所有 VPS 的实时状态与长期统计；客户端（Agent）部署在每台 VPS 上，定时把自身信息签名后上报给服务端。

- **单文件部署**：Go 编写，`CGO_ENABLED=0` 静态编译，无运行时依赖；内置网页、SQLite（纯 Go 实现）与时区库，可直接在 Debian / Ubuntu（amd64 / arm64 / armv7）运行。
- **可信鉴权**：每个节点独立密钥，HMAC-SHA256 签名 + 时间窗 + 一次性 nonce 防重放；失败次数按 IP 限流。
- **长期统计**：分钟级数据保留 7 天，小时级数据默认永久保留；按 **日 / 周 / 月** 统计 CPU、内存、负载、流量、峰值速率和可用率，支持 CSV 导出。
- **流量周期**：每个节点可设置每月重置日与流量配额，首页直接显示本周期已用流量与进度。
- **零依赖 Agent**：直接读取 `/proc`，常驻内存约 10 MB；自动处理重启后计数器清零、服务端宕机期间的流量补记、本机时钟偏差。

## 架构

```
 ┌──────────── VPS ────────────┐                 ┌──────────────── 服务端 ────────────────┐
 │ serverstatus-agent          │  POST /api/report│ serverstatus-server                    │
 │  /proc → 采集 → JSON         │ ───────────────▶ │  鉴权 → 内存实时状态 → 每分钟聚合落盘   │
 │  HMAC-SHA256 签名            │  (每 3 秒)        │  SQLite: 分钟桶(7天) + 小时桶(永久)     │
 └─────────────────────────────┘                  │  内置网页 / JSON API                   │
                                                  └────────────────────────────────────────┘
```

**数据流**：最新一次上报保存在内存里，网页每 2 秒拉取一次。每个节点在内存中累积“当前分钟”的数据，到分钟边界后写入一条分钟记录，并在同一事务里累加到对应的小时记录。日、周、月统计在查询时由小时记录按配置的时区聚合得到，所以修改统计口径不需要迁移数据。平均值以“和 + 样本数”的形式存储，任意粒度合并都不损失精度。

## 安全设计

每个节点有一个随机生成的 256 位密钥，只保存在服务端数据库和该节点的 agent 配置中，不会在网络上传输。每次上报：

```
签名原文 = "PROBE-HMAC-SHA256\nPOST\n/api/report\n<node_id>\n<unix时间戳>\n<随机nonce>\n<hex(sha256(body))>"
X-Probe-Signature = hex(HMAC-SHA256(secret, 签名原文))
```

服务端校验顺序：
1. 时间戳与服务端时间相差不超过 `max_clock_skew`（默认 60 秒）。
2. 用常量时间比较校验 HMAC。签名覆盖请求体哈希，篡改任何字段都会失败。
3. nonce 在时间窗内只能使用一次。超出时间窗的请求在第 1 步就会被拒绝，因此截获的请求无法重放。
4. 同一 IP 鉴权失败 30 次后封禁 10 分钟。

如果 agent 所在机器的时钟有偏差，服务端会在 401 响应中返回自己的时间，agent 会自动补偿并在日志里提示开启 NTP。

> HMAC 只保证**身份与完整性**，不提供**加密**。上报内容（负载、流量等）会以明文传输，生产环境建议使用 HTTPS（内置 TLS 或反向代理）。

网页默认公开只读，展示的信息不包含 IP。如需私有，可执行 `passwd` 设置访问密码。密码以 PBKDF2-SHA256（60 万次迭代）存储，登录会话使用 HMAC 签名的 HttpOnly Cookie；修改密码会使所有旧会话失效。

## 快速开始

### 1. 安装服务端

从 [Releases](../../releases) 下载对应架构的压缩包：

```bash
tar xzf serverstatus_linux_amd64.tar.gz && cd serverstatus_linux_amd64
```

```bash
sudo ./install.sh server --listen :8080
```

安装脚本会：创建系统用户 `serverstatus`，安装 `/usr/local/bin/serverstatus-server`，生成 `/etc/serverstatus/server.yaml`，然后注册并启动 systemd 服务（附带 `ProtectSystem=strict` 等加固项）。

按需编辑配置（尤其是 `timezone`，它决定日/周/月的边界），然后重启：

```bash
sudo systemctl restart serverstatus-server
```

### 2. 添加节点

```bash
sudo serverstatus-server -c /etc/serverstatus/server.yaml node add -name "香港-01" -group "亚洲" -location "Hong Kong" -limit 1T -reset-day 15
```

命令会输出该节点的 `ID`、`Secret`，以及一条可以直接复制的 agent 安装命令。

### 3. 在 VPS 上安装 Agent

```bash
sudo ./install.sh agent --server 'https://status.example.com' --id 'xxxx' --secret 'xxxx'
```

本地没有压缩包时，脚本会自动从 GitHub Release 下载对应架构的版本，所以也可以直接执行：

```bash
curl -fsSL https://raw.githubusercontent.com/Lightmani/ServerStatus/main/deploy/install.sh | sudo bash -s -- agent --server 'https://status.example.com' --id 'xxxx' --secret 'xxxx'
```

查看日志：

```bash
journalctl -u serverstatus-agent -f
```

### 卸载

```bash
sudo ./install.sh uninstall agent
```

`uninstall server --purge` 会同时删除配置和数据库。

## 命令行

```
serverstatus-server [-c config] <command>
  serve                      运行服务端（默认）
  node add -name N [-id ID] [-group G] [-location L] [-sort N] [-reset-day D] [-limit 1T] [-hidden]
  node list                  列出节点
  node edit ID [同上参数]     仅修改显式传入的字段
  node secret ID             轮换密钥（旧密钥立即失效）
  node del [-y] ID           删除节点及其全部历史数据
  passwd [-clear] [-stdin]   设置 / 清除网页访问密码
  config-example             输出示例配置
  version

serverstatus-agent [-c /etc/serverstatus/agent.yaml]
  -server URL -id ID -secret-file PATH -interval 3s   覆盖配置文件
  -once                      采集一次并打印 JSON（排查用）
  环境变量 SERVERSTATUS_SECRET 可代替配置中的 secret
```

节点管理命令直接写数据库，服务端运行时无需重启，10 秒内自动生效。`-hidden` 节点只在设置了密码且已登录时显示。

## 配置

服务端 `/etc/serverstatus/server.yaml`：

```yaml
listen: ":8080"
database: "/var/lib/serverstatus/serverstatus.db"
timezone: "Asia/Shanghai"      # 日/周/月统计边界；"Local" = 系统时区
title: "Server Status"
public_url: ""                 # 仅用于 CLI 输出的安装提示
tls_cert: ""                   # 内置 TLS（证书需对 serverstatus 用户可读）
tls_key: ""
real_ip_header: ""             # 反向代理后填 "X-Real-IP"；直连时务必留空，否则 IP 限流可被伪造
offline_after: 15s
max_clock_skew: 60s
retention:
  minute_days: 7
  hour_days: 0                 # 0 = 永久
```

Agent 配置 `/etc/serverstatus/agent.yaml` 见 [deploy/agent.example.yaml](deploy/agent.example.yaml)。流量统计默认排除 `lo`、`docker*`、`veth*`、`br-*` 等虚拟网卡，避免容器流量被重复计算；可通过 `nic_exclude` 自定义。

### 反向代理（推荐，用于 HTTPS）

Caddy：

```
status.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Nginx：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

使用 Nginx 时，在 server.yaml 中设置 `real_ip_header: "X-Real-IP"`，并把 `listen` 改为 `127.0.0.1:8080`。签名针对的是逻辑路径 `/api/report`，即使挂在子路径下（如 `https://example.com/status/`）也能正常工作。

## 统计口径

| 指标 | 说明 |
|---|---|
| CPU | `/proc/stat` 两次采样之差，(总量 − idle − iowait) / 总量；另外单独给出 steal（超售时偏高）和 iowait |
| 内存 | MemTotal − MemAvailable |
| 硬盘 | 所有物理文件系统之和（ext4 / xfs / btrfs / zfs…，按设备与 ZFS 池去重） |
| 流量 | 服务端根据 agent 上报的累计计数器求差。重启（btime 变化）后从零重新累计；服务端停机期间的流量会在恢复后补记到恢复后的那一分钟 |
| 可用率 | 有数据的分钟数 / 该周期内应有的分钟数（从节点创建时间算起） |
| TCP/UDP | `/proc/net/sockstat{,6}` 的 inuse |

存储开销实测约 280 字节/行：每个节点的分钟数据固定约 2.8 MB（保留 7 天），小时数据每年增长约 2.4 MB。

## 从源码构建

需要 Go 1.24+：

```bash
make test
```

```bash
make build
```

`make build` 的产物在 `bin/`。交叉编译 amd64 / arm64 / armv7 的发布包：

```bash
make release VERSION=v1.0.0
```

发布包输出到 `dist/serverstatus_linux_<arch>.tar.gz`。推送 `v*` 标签后，GitHub Actions 会自动构建并发布 Release。

## HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/report` | Agent 上报（HMAC 鉴权） |
| GET | `/api/config` | 标题、版本、时区、是否需要登录 |
| POST | `/api/login` / `/api/logout` | 网页登录 |
| GET | `/api/nodes` | 全部节点的实时状态与本周期流量 |
| GET | `/api/nodes/{id}/history?range=1h\|6h\|24h\|7d\|30d\|90d\|1y` | 时间序列（列式 JSON） |
| GET | `/api/nodes/{id}/stats?period=day\|week\|month&count=N` | 单节点最近 N 个周期的统计 |
| GET | `/api/stats?period=day\|week\|month&offset=N` | 全部节点在某一周期的统计（offset=0 为当前周期） |
