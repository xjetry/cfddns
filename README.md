# cfddns

Cloudflare DDNS：把本机公网 IPv4 / IPv6 同步到 Cloudflare A / AAAA 记录。

- 周期更新（systemd 常驻）
- 一键安装 / 卸载
- 可选 HTTP 手动触发
- 缺 v4/v6 栈 soft-fail，daemon 不退出

仓库：https://github.com/xjetry/cfddns

---

## 一键安装

> 需要 root。先准备 [API Token](#cloudflare-api-token)（`Zone:DNS:Edit` + `Zone:Zone:Read`）。  
> 下面命令会下载 **latest** 静态二进制并 `systemd enable --now`。

### 自动识别架构（推荐）

只更新 IPv4（把域名和 token 换成你的）：

```bash
curl -fsSL -o /tmp/cfddns "https://github.com/xjetry/cfddns/releases/latest/download/cfddns-linux-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" \
  && chmod +x /tmp/cfddns \
  && sudo /tmp/cfddns -install -4 home.example.com -token 'YOUR_CF_TOKEN'
```

IPv4 + IPv6：

```bash
curl -fsSL -o /tmp/cfddns "https://github.com/xjetry/cfddns/releases/latest/download/cfddns-linux-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" \
  && chmod +x /tmp/cfddns \
  && sudo /tmp/cfddns -install \
       -4 home.example.com \
       -6 home.example.com \
       -token 'YOUR_CF_TOKEN' \
       -period 5m
```

带 HTTP 手动触发（`POST /update` + Bearer）：

```bash
curl -fsSL -o /tmp/cfddns "https://github.com/xjetry/cfddns/releases/latest/download/cfddns-linux-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" \
  && chmod +x /tmp/cfddns \
  && sudo /tmp/cfddns -install \
       -4 home.example.com \
       -token 'YOUR_CF_TOKEN' \
       -port 8080 \
       -passwd 'YOUR_HTTP_SECRET'
```

### 固定架构（amd64 / arm64）

**linux/amd64**

```bash
curl -fsSL -o /tmp/cfddns https://github.com/xjetry/cfddns/releases/latest/download/cfddns-linux-amd64 \
  && chmod +x /tmp/cfddns \
  && sudo /tmp/cfddns -install -4 home.example.com -token 'YOUR_CF_TOKEN'
```

**linux/arm64**

```bash
curl -fsSL -o /tmp/cfddns https://github.com/xjetry/cfddns/releases/latest/download/cfddns-linux-arm64 \
  && chmod +x /tmp/cfddns \
  && sudo /tmp/cfddns -install -4 home.example.com -token 'YOUR_CF_TOKEN'
```

### wget 等价

```bash
wget -qO /tmp/cfddns "https://github.com/xjetry/cfddns/releases/latest/download/cfddns-linux-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" \
  && chmod +x /tmp/cfddns \
  && sudo /tmp/cfddns -install -4 home.example.com -token 'YOUR_CF_TOKEN'
```

安装结果：

| 路径 | 说明 |
|------|------|
| `/usr/local/bin/cfddns` | 二进制 |
| `/etc/cfddns.env` | 配置（`0600`） |
| `cfddns.service` | systemd 单元并已启动 |

### 卸载

```bash
sudo cfddns -uninstall
```

### 日志

```bash
journalctl -u cfddns -f
```

```bash
journalctl -u cfddns -n 100 --no-pager
```

```bash
journalctl -u cfddns --since "1 hour ago"
```

### 装完后手动触发一轮

```bash
sudo cfddns -once
```

HTTP（若装时开了 `-port` / `-passwd`）：

```bash
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

```bash
cfddns -h
```

| 参数 | 环境变量 | 默认 | 说明 |
|------|----------|------|------|
| `-4` | `CFDDNS_V4` | — | IPv4 域名（A） |
| `-6` | `CFDDNS_V6` | — | IPv6 域名（AAAA） |
| `-token` | `CFDDNS_TOKEN` | — | **必填** Cloudflare API Token |
| `-period` | `CFDDNS_PERIOD` | `5m` | 更新周期 |
| `-port` | `CFDDNS_PORT` | 不开启 | HTTP 端口 |
| `-passwd` | `CFDDNS_PASSWD` | — | 与 `-port` 联用，Bearer 密钥 |
| `-once` | — | false | 只跑一轮后退出 |
| `-install` | — | false | 安装 systemd（需 root） |
| `-uninstall` | — | false | 卸载（需 root） |

规则：

- `-4` / `-6` **至少一个**
- 开了 `-port` 必须带 `-passwd`
- 配置合并：defaults → `/etc/cfddns.env` → 进程环境变量 → CLI
- 缺对应 IP 栈：该族失败并打日志，**daemon 不退出**

---

## 行为说明

| 模式 | 行为 |
|------|------|
| 默认 daemon | 启动先更新一次，再按 `-period` 循环；可选 HTTP |
| `-once` | 更新一轮；全部族成功 exit 0，否则非 0 |
| `-install` | 装二进制 + env + unit 并启动 |
| `-uninstall` | 停服务，删 unit / env / 二进制 |

公网 IP 来自 Cloudflare `cdn-cgi/trace`（强制 v4 / v6 拨号）。DNS 始终 **DNS only**（`proxied=false`）。IP 未变则跳过 update。

---

## 从源码构建

```bash
CGO_ENABLED=0 go build -ldflags="-s -w" -o cfddns ./cmd/cfddns
```

交叉编译：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o cfddns-linux-amd64 ./cmd/cfddns
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o cfddns-linux-arm64 ./cmd/cfddns
```

CI：push `main` 构建双架构 Artifact；推送 tag `v*` 发布 Release（`cfddns-linux-amd64` / `cfddns-linux-arm64` / `SHA256SUMS`）。

```bash
git tag v0.1.0 && git push origin v0.1.0
```
