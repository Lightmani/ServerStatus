# ServerStatus

轻量的 VPS 探针：每台 VPS 上运行一个客户端（`ss-agent`），定时把主机状态签名后上报给服务端（`ss-server`），服务端在网页上汇总展示所有节点。

- **零依赖**：纯 Go 标准库，服务端和客户端各是一个静态二进制，网页资源内嵌在服务端里。
- **可信鉴权**：每个客户端持有独立的 Ed25519 私钥，服务端只保存公钥；每次上报都带签名、时间戳和一次性随机数，防伪造、防篡改、防重放。
- **详细信息**：CPU、负载、内存、Swap、各分区硬盘、实时网速与累计流量、TCP/UDP 连接数、进程数、在线时间、系统/内核/架构/虚拟化类型/CPU 型号，以及最近约 10 分钟的 CPU、内存、网速曲线。
- **运维友好**：`SIGHUP` 热加载节点列表（增删、吊销节点无需重启），离线自动标记，网页支持深色模式和手机。

## 快速开始

### 1. 编译

需要 Go 1.22+。

```bash
make build            # 生成 bin/ss-server 和 bin/ss-agent（当前平台）
make dist             # 交叉编译常见 Linux 架构到 dist/
```

客户端目前只支持 Linux（读取 `/proc`、`/sys`）；服务端可运行在任何平台。

### 2. 在每台 VPS 上生成密钥（客户端）

```bash
sudo install -m 755 ss-agent /usr/local/bin/
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ss-agent
sudo install -d -m 750 -o root -g ss-agent /etc/serverstatus
sudo ss-agent keygen -key /etc/serverstatus/agent.key   # 输出一行 base64 公钥，复制它
sudo chown ss-agent:ss-agent /etc/serverstatus/agent.key
```

私钥以 `0600` 权限写入，且**永远不离开这台 VPS**。客户端拒绝加载权限过宽的私钥文件。之后需要再次查看公钥时执行 `ss-agent pubkey -key /etc/serverstatus/agent.key`。

### 3. 在服务端登记公钥

```bash
sudo install -m 755 ss-server /usr/local/bin/
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ss-server
sudo install -d -m 750 -o ss-server -g ss-server /etc/serverstatus
sudo install -m 600 -o ss-server -g ss-server deploy/config.example.json /etc/serverstatus/config.json
```

编辑 `/etc/serverstatus/config.json`，为每台 VPS 添加一项：

```json
{ "id": "hk-01", "name": "香港 01", "location": "HK", "public_key": "<上一步复制的公钥>" }
```

### 4. 启动

服务端：

```bash
sudo cp deploy/ss-server.service /etc/systemd/system/
sudo systemctl enable --now ss-server
```

客户端：

```bash
sudo cp deploy/agent.env.example /etc/serverstatus/agent.env   # 填写 SS_SERVER 和 SS_ID
sudo cp deploy/ss-agent.service /etc/systemd/system/
sudo systemctl enable --now ss-agent
journalctl -u ss-agent -f
```

打开 `http://<服务端>:8080/` 即可看到节点。之后增删节点只需修改 `config.json` 并执行 `sudo systemctl reload ss-server`，无需重启。

## 工作原理

```
┌─────────── VPS ───────────┐                  ┌──────────── 服务端 ────────────┐
│ ss-agent                  │  POST /api/report │ ss-server                      │
│  读取 /proc /sys          │ ───────────────▶ │  1. 校验签名/时间戳/随机数      │
│  每 3 秒采集一次           │   JSON + 签名头   │  2. 清洗数据、存入内存          │
│  私钥签名                  │                  │  3. GET /api/servers → 网页     │
└───────────────────────────┘                  └────────────────────────────────┘
```

### 鉴权协议

每个请求携带四个请求头：

| 请求头 | 内容 |
| --- | --- |
| `X-Probe-Client` | 客户端 ID |
| `X-Probe-Timestamp` | Unix 秒级时间戳 |
| `X-Probe-Nonce` | 16 字节随机数（hex） |
| `X-Probe-Signature` | 对下列规范串的 Ed25519 签名（base64） |

签名的规范串：

```
SSv1\n<用途 "report">\n<客户端ID>\n<时间戳>\n<随机数>\n<hex(sha256(请求体))>
```

服务端依次检查：客户端 ID 已登记且未禁用 → 签名有效 → 时间戳与服务端时间相差不超过 `max_clock_skew_seconds`（默认 60 秒）→ 随机数在时间窗口内未出现过（通过验签后才记录，未认证流量无法撑大缓存）。全部通过后，**服务端只信任签名头中的客户端 ID**，请求体里的任何字段都不能冒充其他节点。

这个设计能防住的：

- **伪造上报**：没有私钥无法生成有效签名；用 A 的私钥冒充 B 会因公钥不匹配而失败。
- **篡改**：请求体哈希在签名内，改一个字节签名就失效。
- **重放**：截获的请求在时间窗口外因时间戳失效，在窗口内因随机数已使用而失效。
- **服务端配置泄露**：配置里只有公钥，泄露后攻击者也无法伪造任何节点。
- **单点吊销**：某台 VPS 被入侵时，只需在配置里删除或 `"disabled": true` 该节点并 `reload`，其他节点不受影响。

需要你自己负责的：

- **传输保密性**：签名保证真实性和完整性，但不加密。走公网时请使用 HTTPS（配置 `tls_cert`/`tls_key`，或放在 Nginx/Caddy 后面），否则监控数据本身可被旁路看到。
- **时钟同步**：客户端时钟偏差超过窗口会被拒绝（服务端会明确返回“检查时钟”的提示）。VPS 上开启 NTP（`timedatectl set-ntp true`）即可。
- **已被入侵的节点**：它仍能用自己的身份上报虚假数据。服务端会对所有字段做长度和数值范围限制，网页只用 `textContent` 渲染并启用严格 CSP，所以它无法借此攻击服务端或浏览器，但数据本身的真假只能靠吊销来处理。

### 网页访问控制

默认网页公开访问（但默认**不展示节点 IP**，见 `show_ip`）。如需登录保护，设置：

```json
"dashboard": { "username": "admin", "password": "一个强密码" }
```

启用后网页和 `/api/servers` 都需要 HTTP Basic Auth；客户端上报不受影响（它用签名鉴权）。Basic Auth 必须配合 HTTPS 使用。

## 配置参考

服务端 `config.json`（未知字段会报错，防止拼写错误被静默忽略）：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `:8080` | 监听地址 |
| `tls_cert` / `tls_key` | 空 | 证书路径，两者都设置时启用 HTTPS |
| `max_clock_skew_seconds` | `60` | 允许的客户端时钟偏差 |
| `offline_after_seconds` | `15` | 超过该时间未上报即标记离线 |
| `trust_proxy_headers` | `false` | 在反向代理后时设为 `true`，从 `X-Forwarded-For` 最右项取真实 IP |
| `show_ip` | `false` | 网页是否展示各节点上报 IP |
| `dashboard.username` / `password` | 空 | 网页 Basic Auth |
| `clients[].id` | 必填 | 节点 ID，`A-Z a-z 0-9 _ . -`，最长 64 |
| `clients[].name` | 同 id | 显示名称 |
| `clients[].location` | 空 | 地区标签 |
| `clients[].public_key` | 必填 | `ss-agent keygen` 输出的公钥 |
| `clients[].disabled` | `false` | 吊销该节点：拒绝其上报并从网页隐藏 |

`listen`、TLS 和 `max_clock_skew_seconds` 修改后需要重启，其余项 `reload` 即生效；配置有误时 reload 会保留旧配置并在日志中报错。

客户端：

```
ss-agent run -server URL -id ID [-key PATH] [-interval 3s]
```

`-server` 和 `-id` 也可以通过环境变量 `SS_SERVER`、`SS_ID` 提供。

## 反向代理示例（Nginx）

```nginx
server {
    listen 443 ssl http2;
    server_name status.example.com;
    # ssl_certificate ...;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

同时在 `config.json` 中设置 `"listen": "127.0.0.1:8080"` 和 `"trust_proxy_headers": true`。签名不包含 URL 路径，因此挂在子路径下（如 `/status/`）也能正常工作，客户端 `-server` 填 `https://example.com/status` 即可。

## 指标口径

- **CPU**：两次采样间 `/proc/stat` 的非空闲时间占比（iowait 计为空闲）。
- **内存**：`MemTotal - MemAvailable`，与 `free` 的 used 一致。
- **硬盘**：真实块设备文件系统（ext4/xfs/btrfs/zfs 等），同一设备只计一次（bind mount、btrfs 子卷不重复计算）。
- **网络**：排除 `lo`、docker/veth/bridge、WireGuard/tun/tailscale 等虚拟接口，避免流量被重复统计。累计流量为开机以来的值，重启后归零。
- **连接数**：TCP 为非 LISTEN 状态的套接字数（IPv4+IPv6），UDP 为全部套接字数。

## 开发

```bash
make test     # go vet + go test -race
```

目录结构：

```
cmd/ss-server        服务端入口（配置加载、SIGHUP 热加载、优雅退出）
cmd/ss-agent         客户端入口（keygen / pubkey / run）
internal/auth        签名与验签、防重放、密钥文件
internal/protocol    上报数据结构及服务端数据清洗
internal/collector   Linux 指标采集（/proc 解析器与单元测试）
internal/server      HTTP 处理、内存存储、网页鉴权
internal/web/static  网页（原生 JS，无构建步骤）
deploy/              示例配置与 systemd 单元
```

## 已知限制

- 状态只存在内存中，服务端重启后历史曲线清空（节点在几秒内重新出现）。
- 没有告警（离线/高负载通知）和按月流量统计。
- 客户端仅支持 Linux。
