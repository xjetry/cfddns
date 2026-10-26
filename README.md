# cfddns

Cloudflare DDNS：把本机公网 IPv4 / IPv6 同步到 Cloudflare A / AAAA 记录。

- 单个静态二进制，支持 systemd（Debian / Ubuntu / CentOS 等）、OpenRC（Alpine）、procd（OpenWrt）
- 周期更新，zone / 记录 ID 缓存，IP 不变时不调 Cloudflare API
- 一键安装 / 升级 / 卸载
- 可选 HTTP 手动触发
- IPv6 可从指定网卡取稳定地址，避开隐私扩展的临时地址
- 缺 v4 / v6 栈 soft-fail，daemon 不退出

仓库：https://github.com/xjetry/cfddns

---

## 一键安装

> 需要 root。先准备 [API Token](#cloudflare-api-token)（`Zone:DNS:Edit` + `Zone:Zone:Read`）。
> `install.sh` 会识别 CPU 架构与 init 系统，下载 **latest** 二进制并执行 `cfddns -install`，服务随即启动并开机自启。

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

`install.sh` 认两个环境变量：`CFDDNS_VERSION=v0.2.0` 装指定版本，`CFDDNS_ASSET=mipsle` 强制产物。

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

校验：从 Release 下载 `SHA256SUMS` 后 `sha256sum -c SHA256SUMS`。

### 安装结果

| init | 二进制 | 配置（`0600`） | 服务定义 | 日志 |
|------|--------|---------------|----------|------|
| systemd | `/usr/local/bin/cfddns` | `/etc/cfddns.env` | `/etc/systemd/system/cfddns.service` | `journalctl -u cfddns -f` |
| OpenRC（Alpine） | `/usr/local/bin/cfddns` | `/etc/cfddns.env` | `/etc/init.d/cfddns` | `tail -f /var/log/cfddns.log` |
| procd（OpenWrt） | `/usr/bin/cfddns` | `/etc/cfddns.env` | `/etc/init.d/cfddns` | `logread -e cfddns -f` |

三种 init 都只负责拉起裸二进制，配置由 cfddns 自己读 `/etc/cfddns.env`。init 系统自动识别；识别不对时加 `-init systemd|openrc|procd`。

### 升级 / 改配置

重新执行同一条安装命令即可。`-install` 不读取旧的 `/etc/cfddns.env`，写入的就是这次传的参数，然后自动 restart 服务，新二进制和新配置立即生效。

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

### 装完后手动触发一轮

```sh
sudo cfddns -once
```

HTTP（若装时开了 `-port` / `-passwd`）：

```sh
curl -sS -H "Authorization: Bearer YOUR_HTTP_SECRET" -X POST http://127.0.0.1:8080/update
```

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
| `-6-iface` | `CFDDNS_V6_IFACE` | — | 优先用该网卡的稳定全局 IPv6；取不到回退 trace |
| `-token` | `CFDDNS_TOKEN` | — | **必填** Cloudflare API Token |
| `-period` | `CFDDNS_PERIOD` | `5m` | 更新周期，最小 `10s` |
| `-port` | `CFDDNS_PORT` | 不开启 | HTTP 端口 |
| `-passwd` | `CFDDNS_PASSWD` | — | 与 `-port` 联用，Bearer 密钥 |
| `-once` | — | false | 只跑一轮后退出 |
| `-install` | — | false | 安装为服务（需 root） |
| `-uninstall` | — | false | 卸载（需 root） |
| `-init` | — | 自动 | `-install` / `-uninstall` 用的 init：`systemd` `openrc` `procd` |

规则：

- `-4` / `-6` **至少一个**；域名统一转小写，须是 RFC 1123 主机名（IDN 用 punycode）
- 开了 `-port` 必须带 `-passwd`
- daemon / `-once` 的配置合并：defaults → `/etc/cfddns.env` → 进程环境变量 → CLI
- `-install` 只认进程环境变量 + CLI，不合并旧的 `/etc/cfddns.env`
- `/etc/cfddns.env` 的值是裸 `KEY=value`：可见 ASCII，不含空白、引号、反斜杠、`#`、`;`；`-token` / `-passwd` 要能装进这个格式
- 缺对应 IP 栈：该族失败并打日志，**daemon 不退出**

---

## 行为说明

| 模式 | 行为 |
|------|------|
| 默认 daemon | 启动先更新一次，再按 `-period` 循环；可选 HTTP |
| `-once` | 更新一轮；全部族成功 exit 0，否则非 0 |
| `-install` | 装二进制 + env + 服务定义，开机自启并 restart |
| `-uninstall` | 停服务、取消自启，删服务定义 / env / 二进制 |

- **IP 探测**：Cloudflare `cdn-cgi/trace`，强制 v4 / v6 拨号，直连不走代理。`-6-iface` 指定时读 `/proc/net/if_inet6`，取该网卡非临时、非弃用的全局单播地址（手动配置的优先），没有则回退 trace；结果的 `source` 字段说明来源。
- **DNS 写入**：记录不存在时创建，DNS only（`proxied=false`，TTL auto）；已存在时用 PATCH 只改 `content`，面板上设的 proxied / TTL / comment / tags 原样保留。
- **缓存**：首次解析后缓存 zone ID 与记录 ID，IP 不变的周期不调 API，IP 变了直接 PATCH；每 12 个周期全量核对一次，面板手改会在核对时被纠正。
- **同名多条记录**：按 ID 排序只管第一条，其余保持原内容，并在日志和 HTTP 结果里给出 `warning`。
- **日志**：created / updated / error 每次打；`unchanged` 只在首轮和状态变化后打一次，稳态不刷日志。
- **HTTP**：`/update` 单次更新 2 分钟超时，连接有读头 / 读 / 空闲超时；更新互斥，等锁超时直接返回 `ok:false`。

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

CI：push `main` 跑 gofmt / vet / staticcheck / `go test -race` 并构建六个架构的 Artifact；推送 tag `v*` 发布 Release（六个二进制 + `SHA256SUMS`）。

```sh
git tag v0.2.0 && git push origin v0.2.0
```
