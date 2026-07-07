# DataHub_health — 个人健康评测转接服务（Go）

接口转接网关（当前服务路由 **hlt**）：
- **对外（下游，hlt）**：`POST /v1/openapi/zlx/querySrmxHLT`，网关信封 `appKey/sign/encryptionType/body` + **MD5 加签**，
  响应 `head{errorCode,logId,time,errorMsg,timestamp} / body{code,msg,uid,reqid,verify,result{range}}`；在此基础上提供 **License 鉴权** 与 **成功查得数统计**（无额度限制）。
  上游评测富对象结果（`hitCount`/`diseaseCategory[]`）整体 **JSON 序列化为字符串**经 `result.range` 透出，客户自行解析。详见 [`docs/API_接口文档与使用手册_hlt.md`](docs/API_接口文档与使用手册_hlt.md)。

> **额度策略（v0.6+）**：已**取消额度限制**——不限制客户调用次数；系统仅**统计每个用户累计成功查得数据的次数**（上游 001 → busiCode 10）。

> **IP 准入（v0.7）**：网关**不再**做全局/每用户 IP 白名单校验；来源 IP 仅写入审计日志。生产环境由**阿里云 ECS 安全组**等网络层控制访问。

> **按域隔离 + demo 治理（v0.9）**：存储按「域」划分——本服务仅 `hlt` 一个域，独占一套 **PostgreSQL 库 + Redis 逻辑库 + license/appKey/secret + 记录**。跨域使用 license 一律鉴权失败（`505004` 账户信息不存在）。生产启动不播种 demo；开发态各域播种互不相同的 demo appKey。启动时另有防呆校验：两个不同的域若配置了同一个数据库或同一个 Redis 逻辑库，服务直接拒绝启动。管理后台为统一管理员登录，按路由标签页管理。

- **对内（上游，按版本路由）**：每个版本各自对接一个上游，归一化为统一的 `UpstreamResult`（`001`查得 /`999`查无）：
  - `hlt` → **商保电子凭证智能服务平台-个人健康评测 / 博思云易**（`health`，`docs/商保电子凭证智能服务平台-个人健康测评-V3.pdf`：
    信封 `appid/data/noise/sign/version`，V2.0 = data **BASE64** + **MD5 大写**签名（`appid=&data=&noise=&key=&version=` 拼接）；
    单次查询内部两步调用——`{baseURL}/100101001` **个人授权备案**（姓名/身份证/手机号 + 保司代码/名称 + 授权文件地址 → `authCode`）
    → `{baseURL}/700101001` **个人健康评测**（`authCode`+`idCard`，默认近 2 年）；
    归一化：`S0000` 且 `hitCount>0` → `001` 查得（message 富对象 JSON 透出 `result.range`）、`hitCount=0` → `999` 查无（无风险）、`EXXXX` → 上游侧错误不计费）。
  保留 `upstream.Router` 抽象，每版本一个单 provider 路由。

## 目录结构（六边形分层）

```
cmd/relay/                 # 入口：装配各层 + 启动 HTTP/后台任务
internal/
├── api/                   # 接入层：requestId/clientIP 中间件、信封/签名提取、handler、admin API + JWT 中间件 + SPA 托管
├── application/           # 编排层：QueryOrchestrator（主流程 + 审计写入）
├── domain/                # 领域层（无框架依赖）
│   ├── model/             #   核心类型（共享，零依赖；含 admin/审计/用户视图）
│   ├── port/              #   出站接口（仓储/上游/密钥/admin/审计等"端口"）
│   ├── auth/              #   License 鉴权 + appKey 校验 + MD5 加签验签
│   ├── quota/             #   成功查得数统计 + 台账 PENDING→BILLED/UNBILLED（无额度限制）
│   ├── billing/           #   计费判定表 + 状态机
│   ├── parse/             #   参数校验/规范化
│   ├── mapping/           #   上游结果→客户 head/body 响应 + errorCode
│   └── admin/             #   管理后台：登录/用户 CRUD/密钥轮换/审计查询
├── infrastructure/        # 适配器
│   ├── upstream/          #   上游路由 + health(个人健康评测)客户端 + 信封签名
│   ├── persistence/memory #   开发用内存实现（默认）
│   ├── persistence/postgres # 生产：license/台账/审计/管理员（PostgreSQL）
│   ├── persistence/redis  #   生产：成功查得数原子计数（Redis INCR + PG 镜像）
│   └── secret/            #   密钥提供者（按 licenseId 动态读取）
├── job/                   # 异步复查 worker（RequeryWorker；health Requery 当前为 stub）
└── common/                # errs(错误码) / reqid / appctx / jwt / ipfilter(仅解析 IP) / mask
web/admin/                 # 管理后台 React + Vite SPA（DESIGN §16）
migrations/                # 建表 DDL（PostgreSQL）：0001 业务 / 0002 管理后台 / 0003 路由统计 / 0004 demo 清理
scripts/                   # mock_health、e2e、recreate_databases 等辅助脚本
test/                      # 固定测试套件（run.ps1 + cases/*.go）
```

依赖箭头始终指向内层：`api → application → domain ← infrastructure`。

## 前置依赖

| 组件 | 版本/说明 | 用途 |
|---|---|---|
| **Go** | 1.25+（见 `go.mod`） | 编译/运行 relay 服务 |
| **Node.js + npm** | 18+ 推荐 | 仅**构建**管理后台 SPA（`web/admin`） |
| **PostgreSQL** | 15+（生产用阿里云 RDS） | license / 台账 / 审计 / 管理员 |
| **Redis** | 6+（生产用阿里云 Redis） | 成功查得数原子计数（PG 镜像） |
| **博思云易上游凭证** | 商务分配 | `versions.hlt.upstream` 的 `appId` / `key` 等 |

> 本项目**不使用** `config.json`，运行时配置全部为 **YAML**，通过环境变量 `CONFIG_FILE` 指定路径（默认 `./config.yaml`）。

### Go 模块镜像（国内 / 阿里云 ECS）

在**国内服务器**（如阿里云 ECS）上，`go mod download` 默认走 `proxy.golang.org`，常会超时（`dial tcp ...:443: i/o timeout`）。需改用国内镜像：

```bash
# 临时生效（当前 shell）
export GOPROXY=https://mirrors.aliyun.com/goproxy/,direct
export GOSUMDB=sum.golang.google.cn

go mod download
```

长期生效（写入 `~/.bashrc` 后 `source ~/.bashrc`）：

```bash
cat >> ~/.bashrc <<'EOF'
export GOPROXY=https://mirrors.aliyun.com/goproxy/,direct
export GOSUMDB=sum.golang.google.cn
EOF
source ~/.bashrc
```

说明：

| 变量 | 含义 |
|---|---|
| `GOPROXY=...,direct` | 优先走阿里云 Go 模块镜像；镜像没有的再直连源站 |
| `GOSUMDB=sum.golang.google.cn` | 校验和数据库也用国内源 |

若仍超时，可换备用镜像：`export GOPROXY=https://goproxy.cn,direct`。仅在内网可信环境且校验和源不可达时，可临时 `export GOSUMDB=off`（不推荐长期使用）。

## 运行（开发）

```bash
# 安装 Go 依赖（国内 ECS 请先设置 GOPROXY，见上文「Go 模块镜像」）
go mod download

# 默认：无 config.yaml 时使用 memory 适配器
go run ./cmd/relay

# 推荐：本地 memory + mock 上游
CONFIG_FILE=config.local.mem.yaml go run ./cmd/relay

# 另开终端启动 mock 个人健康评测上游（:9116）
go run ./scripts/mock_health.go

# 健康检查
curl http://localhost:8080/healthz
```

开发态（memory；PG 由 `scripts/recreate_databases.go` 在 `SEED_DEMO=1` 时播种）为**每个域分别**预置一个独立的 demo license（`secret` 均为 `demo-app-secret`，appKey 按域不同，**跨域不可用**）：

| 域（路由） | demo appKey |
|---|---|
| hlt | `y8909hlt` |

生产（relay 以 postgres 启动）**不播种** demo license。
上游为**商保电子凭证智能服务平台**（`upstream.kind: health`），需在配置文件中设置 `versions.hlt.upstream` 的 `baseURL`/`appId`/`key`/`claimCompanyCode`/`claimCompanyName`/`authFileUrl`（见 `config.example.yaml`）。

## 运行（生产）

### 1. 准备配置文件

仓库内**仅提交** [`config.example.yaml`](config.example.yaml) 作为模板；含真实凭证的文件均在 `.gitignore` 中，需在本机/服务器上自行创建：

```bash
cp config.example.yaml config.aliyun.prod.yaml
# 编辑 config.aliyun.prod.yaml，填入下方「必填项」
```

| 文件 | 是否在仓库 | 用途 |
|---|---|---|
| `config.example.yaml` | ✅ 提交 | 配置模板（无真实密钥） |
| `config.yaml` | ❌ 忽略 | 通用本地/部署配置（默认路径） |
| `config.local.mem.yaml` | ❌ 忽略 | 本地 memory + mock health |
| `config.aliyun.e2e.yaml` | ❌ 忽略 | 阿里云 PG + Redis + mock health（e2e） |
| `config.aliyun.prod.yaml` | ❌ 忽略 | **生产（Ubuntu 部署用此文件）**：独立 PG + Redis + 真实上游 |

生产环境关键配置（完整字段见 `config.example.yaml` / 本地 `config.aliyun.prod.yaml`）：

```yaml
addr: ":8080"                    # 监听地址；前面通常有 Nginx/SLB 做 HTTPS 终结

upstream:
  timeout: "8s"                  # hlt 单次查询含两步上游调用，勿设太小

storage:
  driver: "postgres"             # 生产必须为 postgres
  migrationsDir: "migrations"    # 相对 relay 工作目录；启动时自动跑 DDL

# 存储按域独立：hlt 一套 PG 库 + Redis 逻辑库；每条路由独立上游
versions:
  hlt:
    upstream:
      kind: "health"
      baseURL: "https://<平台地址>/ciras-rest/ins-cl"
      appId: "<平台分配 appid>"
      key: "<appid 对应签名密钥 key>"
      claimCompanyCode: "<保险公司统一社会信用代码>"
      claimCompanyName: "<保险公司名称>"
      authFileUrl: "<授权文件下载地址>"
    database: { host: "<RDS>", name: "datahub_hlt_prod_db", user: "...", password: "..." }
    redis:    { addr: "<Redis>:6379", db: 8, password: "..." }

admin:
  bootstrapUser: "admin"
  bootstrapPass: "<强密码>"       # 首次启动写入 hlt 库 admin_user 表
  jwtSecret: "<随机长字符串>"     # JWT 签名密钥，务必更换
  spaDir: "web/admin/dist"       # 管理后台静态资源目录
```

**必填项清单**（留空或占位符会导致启动失败或无法对外服务）：

| 配置路径 | 说明 |
|---|---|
| `storage.driver` | 必须为 `postgres` |
| `versions.hlt.database.*` | hlt 域 PG 库 |
| `versions.hlt.redis.*` | hlt 域 Redis 逻辑库（与其它项目/域不得复用） |
| `versions.hlt.upstream.*` | 博思云易上游凭证：`baseURL`/`appId`/`key`/`claimCompanyCode`/`claimCompanyName`/`authFileUrl` |
| `admin.bootstrapPass` / `jwtSecret` | 管理后台登录与 JWT（**禁止使用示例占位符**） |

可选：`billing.requeryInterval`（默认 10s）、`admin.tokenTTL`（默认 8h）、`addr`（默认 `:8080`）、
`versions.hlt.upstream.busType`（默认 `2` 核保）/`apiVersion`（默认 `2.0`）/`authPath`/`assessPath`/`assessTypes`。

### 2. 初始化数据库（首次部署，Ubuntu）

relay 启动时会自动执行 `migrations/*.sql` 建表；**首次**需创建 hlt 域的独立生产库并迁移：

```bash
cd /workspace/DataHub_health   # 或你的部署目录，下同

# 按 config.aliyun.prod.yaml 创建 datahub_hlt_prod_db + 迁移
# ⚠️ 会 DROP 旧表后重建，生产已有数据时慎用
# （开发/e2e 需要 demo license 时加 SEED_DEMO=1；生产不要加）
CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/recreate_databases.go
```

仅清空某库旧表、让 relay 下次启动重跑 migrations 时，可对该库执行 [`scripts/recreate_schema.sql`](scripts/recreate_schema.sql)。

### 3. 构建（Ubuntu）

在**仓库根目录**执行（管理后台需先构建，否则 `/admin/` 不可用）：

```bash
cd /workspace/DataHub_health

# 依赖：Go 1.25+、Node.js 18+（仅构建 SPA 时需要）
sudo apt update
sudo apt install -y golang-go nodejs npm   # 若尚未安装；或用官方/ nvm 安装较新版本

# 国内 ECS：Go 模块走阿里云镜像（见上文「Go 模块镜像」）
export GOPROXY=https://mirrors.aliyun.com/goproxy/,direct
export GOSUMDB=sum.golang.google.cn

go mod download

# 管理后台 SPA → web/admin/dist
cd web/admin
npm install
npm run build
cd ../..

# 编译 relay 二进制
go build -o relay ./cmd/relay
chmod +x relay
```

部署目录内需包含（相对 `relay` 工作目录）：

- `relay` — 可执行文件
- `config.aliyun.prod.yaml` — 生产配置（含真实凭证，勿提交 git）
- `migrations/` — 启动时自动迁移
- `web/admin/dist/` — 管理后台静态文件

### 4. 启动生产服务（Ubuntu）

**前台调试（SSH 里临时跑）：**

```bash
cd /workspace/DataHub_health
export CONFIG_FILE=/workspace/DataHub_health/config.aliyun.prod.yaml
./relay
```

**后台运行（简单方式）：**

```bash
cd /workspace/DataHub_health
nohup env CONFIG_FILE=/workspace/DataHub_health/config.aliyun.prod.yaml ./relay \
  >> /var/log/datahub/relay.log 2>&1 &
```

**推荐：systemd 托管（开机自启）：**

```bash
sudo tee /etc/systemd/system/datahub-health.service <<'EOF'
[Unit]
Description=DataHub_health relay
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/workspace/DataHub_health
Environment=CONFIG_FILE=/workspace/DataHub_health/config.aliyun.prod.yaml
ExecStart=/workspace/DataHub_health/relay
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable datahub-health
sudo systemctl start datahub-health
sudo systemctl status datahub-health
```

查看日志：`journalctl -u datahub-health -f`

可选调试：`LOG_LEVEL=debug CONFIG_FILE=config.aliyun.prod.yaml ./relay`

启动后 relay 会依次：存储隔离防呆校验 → 按域（hlt）连接独立 PG → 自动迁移 → 连接独立 Redis → 装配 hlt 上游 → 创建/校验管理员账号 → 监听 HTTP。

**健康检查：**

```bash
curl http://127.0.0.1:8080/healthz          # 应返回 ok
curl http://127.0.0.1:8080/admin/          # 管理后台（建议仅内网访问）
```

**网络与安全（v0.7）：**

- 网关不做 IP 白名单；生产访问控制由**阿里云 ECS 安全组** / SLB 等网络层负责。
- 对外 HTTPS 在 Nginx/SLB 侧终结；relay 默认 HTTP 监听 `:8080`。
- 管理后台 `/admin/` 应仅限内网或 VPN 访问；ECS 安全组勿对公网开放 8080（除非有 SLB/Nginx 反代）。

### 5. 环境与隔离

| 环境 | 配置文件 | PG 库（每域独立） | Redis DB（每域独立） |
|---|---|---|---|
| 开发/e2e | `config.aliyun.e2e.yaml` | `datahub_hlt_db` | 0 |
| **生产（Ubuntu）** | `config.aliyun.prod.yaml` | `datahub_hlt_prod_db` | 8 |

`storage.driver`：`memory`（开发默认）| `postgres`（**生产必须**）。

> 注意：若与 DataHub（经济能力）项目共用同一 Redis 实例，逻辑库编号不得与其占用的 db0/1/3/4（e2e）、db3/4/6/7（生产）重叠。

### 请求示例

下游 MD5 加签见 `docs/API_接口文档与使用手册_hlt.md` 第二章：对 `body` 非空业务参数按 ASCII 升序拼接后追加 `secret` 再 MD5。

```bash
curl -X POST http://localhost:8080/v1/openapi/zlx/querySrmxHLT \
  -H 'Content-Type: application/json' \
  -d '{
    "encryptionType": 1,
    "appKey": "y8909hlt",
    "sign": "<MD5(idCard...mobile...name...secret)>",
    "body": {
      "mobile": "138xxxx1009",
      "idCard": "330xxxxxxxx4312",
      "name": "张三"
    }
  }'
```

成功响应：`{"head":{"errorCode":"0","logId":"<requestId>","time":532,"errorMsg":"success","timestamp":...},"body":{"code":"001","msg":"成功","uid":"<authCode>","reqid":"...","verify":"","result":{"range":"{\"hitCount\":3,\"diseaseCategory\":[...]}"}}}`。
查无（无命中）：`head.errorCode="0"` + `body.code="999"`；网关级错误（鉴权/参数）：只返回 `head`（`errorCode` 非 0 + `errorMsg`），无 `body`。

## 管理后台（Admin Console，DESIGN §16）

面向我方运营的内部控制台：① 查看用户操作记录与上下游日志、累计成功查得数；② 增删用户（无额度配置）；③ 生成/轮换鉴权 `appKey+secret`；④ 按 uuid(appKey)/名称/手机号检索用户与审计记录。

- **后端 API**：`/admin/api/**`（除 `/admin/api/login` 外均需 `Authorization: Bearer <JWT>`）。
- **初始管理员**：配置文件 `admin.bootstrapUser` / `admin.bootstrapPass`（**非**环境变量；e2e 默认 `admin` / `admin12345`）。
  其它：`admin.jwtSecret`、`admin.tokenTTL`（默认 8h）、`admin.spaDir`（默认 `web/admin/dist`）。
- **用户字段**：名称、手机号（列表脱敏展示）、密钥创建时间、授权过期日期（`validTo`）、累计成功查得数。
- **无 IP 白名单管理页**（v0.7 已移除）。

前端（React + Vite SPA）：

```bash
cd web/admin
npm install
# 开发模式（:5173，自动代理 /admin/api → :8080）
npm run dev          # 打开 http://localhost:5173/admin/
# 或构建静态产物，由 Go 服务在 /admin/ 托管
npm run build        # 产物输出到 web/admin/dist；访问 http://localhost:8080/admin/
```

> 安全：`secret` 仅创建/轮换时一次性返回；审计入参（name/idCard/mobile）一律脱敏存储；管理后台应仅限内网/受控网络访问（网络层由 ECS 安全组等控制）。开发期密码用加盐 SHA-256，生产应换 bcrypt/argon2。

## 实现现状

- ✅ 下游契约（hlt：`/v1/openapi/zlx/querySrmxHLT`、`appKey/sign/encryptionType/body` + MD5 加签、`head/body` 信封、`errorCode` 映射）。
- ✅ 上游：商保电子凭证智能服务平台-个人健康评测（博思云易），V2.0 信封（BASE64 + MD5 大写签名），两步调用（授权备案→健康评测），归一化为 `UpstreamResult`（`001`命中/`999`无命中）；保留 `upstream.Router` 抽象便于扩展。
- ✅ 成功查得数统计（**仅查得数据 busiCode=10 计数**，无额度拦截）、台账状态机、requestId 追踪（`head.logId`）、建表 DDL。
- ✅ 持久化：`memory`（开发）与 `postgres`+`redis`（生产/e2e）。
- ✅ 管理后台：管理员登录（JWT）、用户 CRUD（手机号/密钥时间/过期日期、检索）、`appKey/secret` 生成与轮换、审计日志（含 `?q=` 关键字过滤）、React+Vite SPA。
- ✅ 固定测试套件（`test/run.ps1`；结果输出 `test_res/<date>/`）。
- 🚧 待完善：
  - 上游 **V3.0 国密**（SM2 签名 + SM4 加密）未实现，当前仅支持 V2.0（BASE64 + MD5）；平台若强制 3.0 需补国密实现。
  - health `Requery` 当前为 stub（`Reachable=false`），RequeryWorker 对该上游暂无实际复查能力（上游以 `noise` 判重，重试天然幂等）。
  - `license.valid_to` 已存储并在后台展示，鉴权目前仅检查 `status==ACTIVE`（未按日期自动过期）。
  - `license.rate_limit` 列存在但代码未读取。
  - 密钥列 `app_secret_enc` 开发/e2e 为明文存储（生产应接入 KMS/加密）。

## 测试

```powershell
powershell -ExecutionPolicy Bypass -File .\test\run.ps1
powershell -ExecutionPolicy Bypass -File .\test\run.ps1 -ConfigFile config.local.mem.yaml
```

详见 [`test/README.md`](test/README.md)。
