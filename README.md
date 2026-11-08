# cfddns

Cloudflare DDNS：把本机公网 IPv4 / IPv6 同步到 Cloudflare A / AAAA 记录。

- 单个静态二进制，支持 systemd（Debian / Ubuntu / CentOS 等）、OpenRC（Alpine）、procd（OpenWrt）
- 周期更新，zone / 记录 ID 缓存，IP 不变时不调 Cloudflare API
- 一键安装 / 升级 / 卸载
- 可选 HTTP 手动触发
- 可选运行状态文件（JSON）；`-inspect` 只读查询 Cloudflare 当前记录
- IPv6 可从指定网卡取稳定地址，避开隐私扩展的临时地址
- 缺 v4 / v6 栈 soft-fail，daemon 不退出

仓库：https://github.com/xjetry/cfddns

---

## 一键安装

> 需要 root。先准备 [API Token](#cloudflare-api-token)（`Zone:DNS:Edit` + `Zone:Zone:Read`）。
> `install.sh` 会识别 CPU 架构与 init 系统，下载 **latest** 二进制、按 Release 的 `SHA256SUMS` 校验后执行 `cfddns -install`，服务随即启动并开机自启。下载落在 `/usr/local/bin`（或 `/usr/bin`）而不是 `/tmp`，所以 `/tmp` 挂成 `noexec` 也不影响；需要 `curl` 或 `wget`，以及 `sha256sum`。

只更新 IPv4（把域名和 token 换成你的）：

```sh
wget -qO- https://raw.githubusercontent.com/xjetry/cfddns/main/install.sh \
  | sh -s -- -4 home.example.com -token 'YOUR_CF_TOKEN'
```

curl 等价：

```sh
curl -fsSL https://raw.githubusercontent.com/xjetry/cfddns/main/install.sh \
  | sh -s -- -4 home.example.com -token 'YOUR_CF_TOKEN'
```

IPv4 + IPv6，IPv6 取 `eth0` 的稳定地址：

```sh
wget -qO- https://raw.githubusercontent.com/xjetry/cfddns/main/install.sh \
  | sh -s -- -4 home.example.com -6 home.example.com -6-iface eth0 -token 'YOUR_CF_TOKEN' -period 5m
```

带 HTTP 手动触发（`POST /update` + Bearer）：

```sh
wget -qO- https://raw.githubusercontent.com/xjetry/cfddns/main/install.sh \
  | sh -s -- -4 home.example.com -token 'YOUR_CF_TOKEN' -port 8080 -passwd 'YOUR_HTTP_SECRET'
```

`install.sh` 认两个环境变量：`CFDDNS_VERSION=v0.3.1` 装指定版本，`CFDDNS_ASSET=mipsle` 强制产物。

### 手动下载

| 产物 | 适用 |
|------|------|
| `cfddns-linux-amd64` | x86_64 服务器 / VPS / x86 OpenWrt |
| `cfddns-linux-arm64` | aarch64：树莓派 3/4/5（64 位系统）、ARM VPS、MT7622 / MT7981 / MT7986 / IPQ807x 路由器 |
| `cfddns-linux-armv7` | 32 位 ARMv7：树莓派 2/3（32 位系统）、IPQ40xx 路由器 |
| `cfddns-linux-armv6` | 树莓派 1 / Zero |
| `cfddns-linux-mipsle` | MT7620 / MT7621（ramips）路由器 |
| `cfddns-linux-mips` | ath79 等大端 MIPS 路由器 |

```sh
wget -O /tmp/cfddns https://github.com/xjetry/cfddns/releases/latest/download/cfddns-linux-amd64 \
  && chmod +x /tmp/cfddns \
  && sudo /tmp/cfddns -install -4 home.example.com -token 'YOUR_CF_TOKEN'
```

校验：从 Release 下载 `SHA256SUMS` 后 `sha256sum -c SHA256SUMS`（`install.sh` 自动做这一步）。

### 安装结果

| init | 二进制 | 配置（`0600`） | 服务定义 | 日志 |
|------|--------|---------------|----------|------|
| systemd | `/usr/local/bin/cfddns` | `/etc/cfddns.env` | `/etc/systemd/system/cfddns.service` | `journalctl -u cfddns -f` |
| OpenRC（Alpine） | `/usr/local/bin/cfddns` | `/etc/cfddns.env` | `/etc/init.d/cfddns` | `tail -f /var/log/cfddns.log` |
| procd（OpenWrt） | `/usr/bin/cfddns` | `/etc/cfddns.env` | `/etc/init.d/cfddns` | `logread -e cfddns -f` |

三种 init 都只负责拉起裸二进制，配置由 cfddns 自己读 `/etc/cfddns.env`。init 系统自动识别；识别不对时加 `-init systemd|openrc|procd`。

OpenRC 的 `/var/log/cfddns.log` 由 supervise-daemon 直接写文件，cfddns 不做轮转；稳态不产生日志，持续失败时同一错误只在出现时打一次、之后每 12 轮打一次，所以文件增长有界。需要更严格的控制可自行配 logrotate。

### 升级 / 改配置

重新执行同一条安装命令即可。`-install` 不读取旧的 `/etc/cfddns.env`，写入的就是这次传的参数，然后自动 restart 服务，新二进制和新配置立即生效。三个文件都是先写临时文件再原子替换，所以重装会把 `/etc/cfddns.env` 恢复成 `0600` root 所有，不继承之前被改松的权限。

只改配置：编辑 `/etc/cfddns.env` 后重启：

```sh
sudo systemctl restart cfddns        # systemd
sudo rc-service cfddns restart       # OpenRC
/etc/init.d/cfddns restart           # OpenWrt
```

### 卸载

```sh
sudo cfddns -uninstall
```

停服务或取消自启失败时卸载会中止、不删文件，以免留下一个没人管的进程却报告"已卸载"；只有服务定义已经不在（上一次卸载中途失败）时才忽略这类错误直接清理残留。

### 装完后手动触发一轮

服务启动时已经立刻跑过一轮，通常不必再手动触发。要手动跑：

```sh
sudo cfddns -once
```

和正在运行的服务同时跑是安全的：记录不存在时两边都可能去创建，cfddns 创建后会复读一次，发现并发产生的同名记录时删掉自己刚建的那条、只保留 ID 最小的一条。

HTTP（若装时开了 `-port` / `-passwd`）：

```sh
curl -sS -H "Authorization: Bearer YOUR_HTTP_SECRET" -X POST http://127.0.0.1:8080/update
```

### 查看 Cloudflare 当前记录

```sh
sudo cfddns -inspect
```

只读，不改记录，输出 JSON，见 [状态文件与 inspect](#状态文件与-inspect)。

---

## Cloudflare API token

在 [Cloudflare API Tokens](https://dash.cloudflare.com/profile/api-tokens) 创建，权限至少：

| Permission | Access |
|------------|--------|
| Zone → DNS | Edit |
| Zone → Zone | Read |

Token 范围限定到要更新记录所在的 Zone。

---

## 参数一览

```sh
cfddns -h
```

| 参数 | 环境变量 | 默认 | 说明 |
|------|----------|------|------|
| `-4` | `CFDDNS_V4` | — | IPv4 域名（A） |
| `-6` | `CFDDNS_V6` | — | IPv6 域名（AAAA） |
| `-6-iface` | `CFDDNS_V6_IFACE` | — | 优先用该网卡的稳定全局 IPv6；取不到回退 trace。不指定时 AAAA 跟随内核选的出口源地址，开了隐私扩展（RFC 4941）的系统上那是每天轮换的临时地址 |
| `-token` | `CFDDNS_TOKEN` | — | **必填** Cloudflare API Token |
| `-period` | `CFDDNS_PERIOD` | `5m` | 更新周期，最小 `10s` |
| `-port` | `CFDDNS_PORT` | 不开启 | HTTP 端口 |
| `-passwd` | `CFDDNS_PASSWD` | — | 与 `-port` 联用，Bearer 密钥 |
| `-once` | — | false | 只跑一轮后退出 |
| `-inspect` | — | false | 只读查询 Cloudflare 当前记录，输出 JSON 后退出 |
| `-status-file` | — | 不开启 | 运行状态 JSON 写到该路径；仅 daemon / `-once` |
| `-install` | — | false | 安装为服务（需 root） |
| `-uninstall` | — | false | 卸载（需 root） |
| `-init` | — | 自动 | `-install` / `-uninstall` 用的 init：`systemd` `openrc` `procd` |

规则：

- `-4` / `-6` **至少一个**；域名统一转小写，须是 RFC 1123 主机名（IDN 用 punycode）
- 开了 `-port` 必须带 `-passwd`
- daemon / `-once` / `-inspect` 的配置合并：defaults → `/etc/cfddns.env` → 进程环境变量 → CLI；某一层"设置了但为空"也算设置，会清掉下层的值（`CFDDNS_PORT=` 等于关掉 HTTP）
- `-install` 只认进程环境变量 + CLI，不合并旧的 `/etc/cfddns.env`；`-uninstall` 什么配置都不读
- `-status-file` 只是运行参数：没有环境变量，`-install` 不写进 `/etc/cfddns.env`，生成的服务定义也不带它
- `-inspect` 不能与 `-once` / `-install` / `-uninstall` 同用；`-status-file` 不能与 `-inspect` / `-install` / `-uninstall` 同用
- 命令行只接受 flag，多出来的位置参数直接报错（`flag` 解析遇到第一个非 flag 就停，后面的 `-once` 会被静默丢掉，所以不允许）
- `/etc/cfddns.env` 的值是裸 `KEY=value`：可见 ASCII，不含空白、引号、反斜杠、`#`、`;`；`-token` / `-passwd` 要能装进这个格式
- 缺对应 IP 栈：该族失败并打日志，**daemon 不退出**

---

## 行为说明

| 模式 | 行为 |
|------|------|
| 默认 daemon | 启动先更新一次，再按 `-period` 循环；可选 HTTP |
| `-once` | 更新一轮；全部族成功 exit 0，否则非 0 |
| `-inspect` | 只读查询一次，JSON 写到 stdout；配置有效即 exit 0 |
| `-install` | 装二进制 + env + 服务定义，开机自启并 restart |
| `-uninstall` | 停服务、取消自启，删服务定义 / env / 二进制；停服务失败则中止不删 |

- **IP 探测**：Cloudflare `cdn-cgi/trace`，先试 `1.1.1.1`（v6 为 `2606:4700:4700::1111`），不通再试 `www.cloudflare.com`；都强制 v4 / v6 拨号，直连不走代理；记住上次成功的端点，下轮先试它。返回的地址族与请求不符（经 NAT64 / 隧道等）时该族报错并尝试下一端点，不会把 IPv6 写进 A 记录。`-6-iface` 指定时读 `/proc/net/if_inet6`，取该网卡非临时、非弃用的全局单播地址（手动配置的优先），没有则回退 trace；结果的 `source` 字段说明来源。
- **DNS 写入**：记录不存在时创建，DNS only（`proxied=false`，TTL auto），创建后复读一次，若另一个 cfddns 进程同时也创建了同名记录则删掉自己刚建的、保留 ID 最小的；已存在时用 PATCH 只改 `content`，面板上设的 proxied / TTL / comment / tags 原样保留。
- **缓存**：首次解析后缓存 zone ID 与记录 ID，IP 不变的周期不调 API，IP 变了直接 PATCH；每 12 个周期全量核对一次，面板手改会在核对时被纠正。PATCH 返回 404（记录被删或重建）才重新全量解析；限流 / 5xx / 网络错误只报错并保留缓存，下一轮重试那一次 PATCH。
- **同名多条记录**：按 ID 排序只管第一条，其余保持原内容，并在日志、HTTP 结果、状态文件和 `-inspect` 输出里给出 `warning`；日志在 warning 出现和消失时各打一次。
- **日志**：created / updated 每次打；`unchanged` 和 error 只在状态变化时打（首轮、恢复、错误文本变了）；同一错误持续时每 12 轮再打一次并注明已连续多少轮。稳态不刷日志。
- **HTTP**：`/update` 单次更新 2 分钟超时，连接有读头 / 读 / 空闲超时；更新互斥，等锁超时直接返回 `ok:false`。`Authorization` 的 `Bearer` 不区分大小写。
- **内存**：GOMAXPROCS=1；每轮结束关闭空闲连接并把堆页归还 OS；API 客户端只用 HTTP/1.1。见[内存](#内存)。

---

## 状态文件与 inspect

两者输出都不含 Token，时间都是 RFC 3339 UTC，带小数秒（最多 9 位，末尾 0 省略）。

### 状态文件

默认关闭，显式传 `-status-file <path>` 才写。OpenWrt 打包（后续的 `cfddns-openwrt` feed）会以 `-status-file /var/run/cfddns.status` 运行 daemon，该路径在 tmpfs，不写 Flash。

- 每轮结束后原子替换（同目录临时文件 + rename），权限 `0644`，读到的总是完整 JSON
- 首轮前删掉旧文件；daemon 正常退出（SIGINT / SIGTERM）时删除；`-once` 保留最后一轮结果
- 写入 / 删除失败只打日志，不影响更新结果和退出码，daemon 不退出

内容（实际为单行）：

```json
{
  "version": 1,
  "started_at": "2026-10-27T08:00:00.412345678Z",
  "written_at": "2026-10-27T08:05:01.734512Z",
  "period": "5m0s",
  "families": [
    {
      "family": "ipv4",
      "domain": "home.example.com",
      "record_type": "A",
      "detection": {"at": "2026-10-27T08:05:00.218734Z", "ip": "203.0.113.10", "source": "trace", "error": ""},
      "execution": {"at": "2026-10-27T08:05:01.733901Z", "action": "unchanged", "error": "", "warning": ""}
    }
  ]
}
```

- `detection` / `execution` 在该族尚无对应结果时不出现（omitempty），不是 `null`
- `detection`：最近一次 IP 探测；探测失败时 `ip` 为空、不保留旧 IP，`execution.action` 为 `error` 而 `execution.error` 为空，错误原因只在 `detection.error`
- `execution`：最近一次同步完成，`action` 为 `created` / `updated` / `unchanged` / `error`；`unchanged` 也是已执行
- HTTP 触发等锁超时只更新 `execution`，`detection` 保持上一次探测

### inspect

```sh
sudo cfddns -inspect
```

- 配置合并与 `-once` 相同（要读 `0600` 的 `/etc/cfddns.env`，所以一般要 root）
- 只调 zone / 记录查询接口，不创建、不 PATCH、不触发更新，也不用 daemon 的缓存
- 各族独立查询，整体 2 分钟超时
- 同名多条记录按更新时的规则取 ID 排序第一条，并给出 `warning`

stdout（实际为单行）：

```json
{
  "ok": true,
  "results": [
    {"family": "ipv4", "domain": "home.example.com", "record_type": "A", "found": true, "content": "203.0.113.10", "queried_at": "2026-10-27T08:10:00.531208Z", "warning": "", "error": ""}
  ]
}
```

- 记录不存在：`found:false`、`content` 为空；这是成功查询，`ok` 不会因此变 false
- zone / 记录查询失败：该族 `error` 有值、`ok:false`，另一族仍会返回结果；两族共享整体 2 分钟超时，超时时可能两族都带 `error`
- 配置有效就输出 JSON 并 exit 0，即使 `ok:false`，调用方看 `ok` 和各族 `error`；配置或参数错误打到 stderr 并非 0 退出，不输出 JSON

---

## 内存

daemon 稳态常驻（linux/amd64，1 CPU，真实 Cloudflare 流量，每轮结束后释放）：

| | RSS | 匿名页 | 二进制映射页 |
|---|---|---|---|
| 优化前 | 14.6 MB | 7.5 MB | 7.1 MB |
| 现在 | 12.0 MB | 5.0 MB | 7.0 MB |

匿名页是真正占用 RAM 的部分；二进制映射页大小约等于二进制本身，内核缺内存时可直接回收。32 位的 mipsle / armv7 指针减半，绝对值更低。每轮周期内的瞬时峰值由 TLS 握手决定，约比稳态高 2.5 MB。

RAM 特别紧的路由器可以不常驻 daemon，改用 cron 跑一次性模式，两次运行之间不占内存。先正常 `-install` 生成配置，再停掉服务并加 cron：

```sh
/etc/init.d/cfddns stop && /etc/init.d/cfddns disable     # OpenWrt
echo '*/5 * * * * /usr/bin/cfddns -once' >> /etc/crontabs/root && /etc/init.d/cron restart
```

`-once` 每次都完整解析 zone 和记录（无跨进程缓存），5 分钟一次远低于 Cloudflare 限流。

---

## 从源码构建

```sh
CGO_ENABLED=0 go build -ldflags="-s -w" -o cfddns ./cmd/cfddns
```

交叉编译：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o cfddns-linux-amd64 ./cmd/cfddns
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o cfddns-linux-arm64 ./cmd/cfddns
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o cfddns-linux-armv7 ./cmd/cfddns
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -ldflags="-s -w" -o cfddns-linux-mipsle ./cmd/cfddns
```

CI：push `main` 跑 gofmt / vet / staticcheck / `go test -race` 并构建六个架构的 Artifact；推送 tag `v*` 复用同一套检查与构建后发布 Release（六个二进制 + `SHA256SUMS`）。手动触发 Release 并填入 tag 时，检出并构建的是那个 tag 的源码，与从哪个分支触发无关。

```sh
git tag v0.3.1 && git push origin v0.3.1
```
