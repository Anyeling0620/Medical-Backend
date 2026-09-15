# Medical-Web-Backend

管理端（Web）、患者端（小程序）与匿名公开查询共用的统一 Go API 服务，接口契约以
[spec/04-api-contract.md](../spec/04-api-contract.md) 为准。

本文只讲两件事：**怎么部署**、**出问题怎么查**。

## 部署

### 1. 前置条件

| 依赖 | 要求 |
| --- | --- |
| Go | `go.mod` 声明 1.26（当前 1.26.5） |
| Docker | 容器部署需要 Docker Engine 24+ 与 Compose v2 |
| PostgreSQL | 现有 `hospital` schema，账号需要读写权限 |
| Redis | 登录会话、幂等键、公开域读缓存 |
| MinIO | 医生照片等对象存储，桶要能被端上直接读取 |
| 外部服务 | 微信 code2Session、支付宝当面付（预下单 / 查询 / 异步通知验签） |

服务启动不建表、不改表：表结构变更先写进 `spec/`，再由数据库维护者手工执行。

### 2. 配置

环境变量与默认值集中在 `internal/config/config.go`，本地模板是仓库根目录的 `.env.example`：

```bash
cp .env.example .env    # .env 已被 git 忽略，不要提交
```

`.env.example` 顶部有一份上线前确认清单，重点项：

| 变量 | 说明 |
| --- | --- |
| `APP_ENV` | 生产必须是 `production`。`development` 会放行「患者直接提交 openid 登录」的测试通道；改成 `production` 后微信凭据缺失会直接启动失败 |
| `HTTP_HOST` / `HTTP_PORT` | 默认 `0.0.0.0:9080`，必须与 `deploy/Dockerfile` 的 `EXPOSE`、`deploy/docker-compose.yaml` 的端口映射一致 |
| `PGSQL_ADDR` / `PGSQL_DATABASE` / `PGSQL_SCHEMA` / `PGSQL_USERNAME` / `PGSQL_PASSWORD` | 线上库为 `hospital` schema |
| `REDIS_ADDR` / `REDIS_PASSWORD` / `REDIS_DB` | 会话与幂等存储；不可用时登录、下单会失败 |
| `MINIO_ENDPOINT` / `MINIO_USE_SSL` / `MINIO_BUCKET` | 容器内**不能填 `localhost`**（那是容器自己），要填宿主或公网可达地址 |
| `JWT_SECRET` | 生产必须换成足够长的随机串，不要沿用模板空值或代码默认值 |
| `JWT_COOKIE_SECURE` | 站点走 HTTPS 才设 `true`；IP + HTTP 的站点设 `true` 会让浏览器丢弃登录 Cookie，管理端直接登不进去 |
| `WECHAT_APPID` / `WECHAT_SECRET` | 患者端静默登录依赖，生产必填 |
| `ALIPAY_APP_ID` / `ALIPAY_APP_PRIVATE_SECRET` / `ALIPAY_PUBLIC_SECRET` / `ALIPAY_GATEWAY_URL` / `ALIPAY_SELLER_IDS` | 支付宝当面付；凭据缺失时支付接口返回 `502 PAYMENT_PROVIDER_UNAVAILABLE` |
| `NOTIFY_URL` | 支付宝异步通知地址：必须公网可达、HTTPS、不带查询参数；留空则降级为主动查询 |
| `CACHE_ENABLED` / `CACHE_SCHEDULE_TTL` / `PUBLIC_CATALOG_CACHE_*` | 公开域读缓存；TTL 上限由代码收敛在 10 秒内 |
| `ORDER_EXPIRY_WORKER_ENABLED` / `ORDER_EXPIRY_INTERVAL` / `ORDER_EXPIRY_ORDER_TIMEOUT` | 订单过期收口任务；关闭后未付款订单会一直占号源 |

支付宝 PEM 是多行值，写进 `.env` 时必须整段用一对双引号包住，否则 godotenv 解析失败、服务起不来。

### 3. 本地运行与自检

```bash
go run ./cmd/server
curl http://127.0.0.1:9080/                  # {"service":"medical-backend","status":"ok"}
curl http://127.0.0.1:9080/api/v1/health     # {"status":"ok|degraded","dependencies":{...}}

go build ./...
go vet ./...
go test ./...
```

`/api/v1/health` 的 `status` 为 `degraded` 表示有依赖没连上，`dependencies` 里能直接看到是哪一个。

集成测试默认跳过（连不上库或网关时同样是 skip，不会把 `go test ./...` 变红），需要显式打开开关：

| 开关 | 覆盖范围 |
| --- | --- |
| `PGSQL_INTEGRATION_TEST=1` | PostgreSQL 读写集成测试（就诊卡、挂号、病历、排班、支付收口） |
| `PAYMENT_DB_E2E=1` | 支付仓储 SQL 的真实库验证 |
| `ALIPAY_E2E=1` | 支付宝真实网关联调（默认不访问外网） |
| `PAYMENT_PROBE_GATEWAYS=1` | 支付宝网关只读探测 |
| `REGISTRATION_IT_SWEEP_FIXTURES=1` | 按夹具标记做一次全局清理（兜底巡检） |

辅助脚本：

```bash
# 公开域读接口轻量压测（只用标准库，输出 RPS 与 P50/P90/P95/P99）
go run ./scripts/loadtest -base http://127.0.0.1:9080 -path /api/v1/public/departments -concurrency 32 -duration 20s

# 手工开医生账号：scripts/seed_doctor_account.sql，用 psql 变量传 username / doctor_id / password_hash
# 散列用任意 bcrypt 工具生成（同目录的 generatePasswordHash.go 不是 main 包，go run 起不来）
```

支付联调不需要真实资金：把 `ALIPAY_GATEWAY_URL` 指向本工作区的 `Medical-Mock-Alipay`，
预下单返回的二维码链接在浏览器打开即视为扫码，默认 5 秒后自动支付成功并回调本服务；
两侧配置怎么对齐见该仓库 README。

### 4. 容器部署

```bash
# 必须在仓库根目录构建：Dockerfile 以根目录的 go.mod/go.sum 与整个模块为构建上下文
docker build -f deploy/Dockerfile -t medical-backend:latest .

# 服务器上准备配置与编排
mkdir -p /opt/medical
cp .env.example /opt/medical/backend.env    # 按实际值逐项填写
docker compose -f deploy/docker-compose.yaml up -d
docker compose -f deploy/docker-compose.yaml ps
curl -fsS http://127.0.0.1:9080/api/v1/health
```

- 镜像基座是 `scratch`，只带二进制和 CA 证书；改端口要同时改 `Dockerfile` 的 `EXPOSE`、compose 端口映射和 `HTTP_PORT`。
- compose 读 `/opt/medical/backend.env`，并加入已存在的外部网络 `nginx_default`（该网络不存在时 compose 直接报错）。
- 容器端口只绑 `127.0.0.1:9080`，公网入口由宿主机 nginx 承担，站点配置见 `deploy/nginx.conf`：
  `/api/` 原样转发（`proxy_pass` 结尾**不能带 `/`**）、静态目录 `/var/www/medical-web`、
  SPA 回退目标 `/_shell.html`、`/assets/` 单独 `try_files $uri =404`。

## 调试

### 排查顺序

1. `GET /api/v1/health`：先分清是「依赖没连上」还是「单个业务域的问题」。
2. 看启动日志：依赖连接结果、`APP_ENV=development` 告警、支付宝凭据缺失告警、公开排班缓存是否启用、
   订单收口任务是否启动。这些告警都代表「服务能起但功能降级」。
3. 按域名缩小范围：`/api/v1/mis/*`、`/api/v1/catalog/*`、`/api/v1/schedule/*`、`/api/v1/medical-records/*`
   只收管理端令牌（`realm=mis`）；`/api/v1/patient/*` 只收患者令牌（`realm=patient`）；
   `/api/v1/registrations/*`、`/api/v1/payments/*` 是双 realm 共享路由（管理端令牌要带对应权限编码，
   患者令牌只能访问自己的资源）；`/api/v1/public/*` 匿名只读；支付宝异步通知
   `/api/v1/payments/alipay/notify` 是 provider 回调入口，不读用户令牌。401 先怀疑 realm 用错，403 看权限编码。
4. 响应统一带字符串 `code` 与 `message`，按 `code` 分流；端上都会发 `X-Request-Id`，
   可以直接拿它到后端访问日志里对同一次请求。

### 常见现象

| 现象 | 排查方向 |
| --- | --- |
| 管理端登录接口 200，随后立刻被判未登录并跳回登录页 | `JWT_COOKIE_SECURE` 与站点协议不一致（HTTP 站点必须是 `false`）；管理端必须与后端同源（走 nginx 反代），跨站时 Cookie 不会带上 |
| 容器启动即退出，日志 `WECHAT_APPID and WECHAT_SECRET must be configured` | `APP_ENV=production` 时微信凭据必填 |
| 支付接口 502 `PAYMENT_PROVIDER_UNAVAILABLE` | 未配置 `ALIPAY_APP_ID` / `ALIPAY_APP_PRIVATE_SECRET`，或 `ALIPAY_GATEWAY_URL` 不可达 |
| 服务起不来，报 `unexpected character ... in variable name` | 支付宝 PEM 的多行值没用一对双引号包住，godotenv 解析失败 |
| 支付宝异步通知一直验签失败 | `ALIPAY_PUBLIC_SECRET` 必须是支付宝（或 mock）**公钥**原文，而不是自己的私钥 |
| 医生照片、诊疗材料打不开 | 容器里 `MINIO_ENDPOINT` 不能是 `localhost`；桶名要与前端 `VITE_MINIO_BUCKET` 一致 |
| 未付款订单一直占号源、重新挂号被判重 | 订单收口任务没在跑：`ORDER_EXPIRY_WORKER_ENABLED=false`、PostgreSQL 不可用、支付宝凭据无效，三者任一都会让它不启动，启动日志里有告警 |
| 公开域显示的余量与数据库直接查询不一致 | 公开查询走 8 秒级短 TTL 缓存（上限 10 秒），属预期；Redis 不可用时自动降级直查 |
| 宿主机 `curl 127.0.0.1:9080` 通、公网 404/502 | nginx 配置问题，优先检查 `proxy_pass` 是否带了尾斜杠 |

### 约定提醒

- `.env` 已被 git 忽略，真实密钥、PEM 原文、数据库口令不要写进代码、文档或提交记录。
- 日志、缓存和错误信息里不得出现完整身份证号、完整手机号或支付私钥。
