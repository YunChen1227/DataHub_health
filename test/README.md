# DataHub_health 固定测试套件（test/）

一套可重复执行的全链路测试。每次需要测试时，运行根目录入口脚本即可：它会启动本地 mock 上游 + relay（连接你在阿里云的线上 PostgreSQL + Redis），依次跑完 `test/cases/` 下的所有脚本，把每个脚本的结果写进**以当天日期命名的子目录**，最后汇总成一份易读的 `REPORT.md`。

## 一键运行

```powershell
# 在 DataHub_health 目录下
pwsh ./test/run.ps1
```

可选参数：

```powershell
pwsh ./test/run.ps1 -ConfigFile config.aliyun.e2e.yaml   # 默认即此，连线上 PG+Redis
pwsh ./test/run.ps1 -ConfigFile config.local.mem.yaml    # 纯本地 memory 模式
```

运行后结果在：`test_res/<YYYY-MM-DD>/`，其中：

- `<suite>.json`：每个脚本的结构化结果（机器可读）。
- `<suite>.log`：每个脚本的完整 stdout（人类可读）。
- `relay.log` / `mock_health.log`：服务端日志，排错用。
- `REPORT.md`：**最终汇总报告**，逐接口/功能给出"通过/失败/跳过 + 原因"。

## 架构与连通性

- relay 以 `CONFIG_FILE=config.aliyun.e2e.yaml` 启动，存储后端 = **线上阿里云 PostgreSQL + Redis**；上游默认指向本地 mock（health :9116），保证主测试矩阵确定可重复。
- 存储按「域」划分：本服务仅 hlt 一个域库，license 与统计/日志按路由独立；任何域的 license（含 demo）在其它域的路由上一律鉴权失败（`505004`）。
- relay 启动会自动跑迁移（`0001`~`0004`）。demo license 由 `SEED_DEMO=1 go run ./scripts/recreate_databases.go` 按域播种：hlt=`y8909hlt`，`secret` 为 `demo-app-secret`（harness `AppKeyFor(version)`）。
- `00_connectivity` 会**直接** ping 线上 PG + Redis，确认本机确实连得上。

## 对线上数据的影响（已尽量降到最低）

- 计数类断言用"前后差值"，不依赖绝对值；demo license 的 `serviceUsed` 会随每次成功查得累计（正常现象）。
- `06_admin_crud` 创建的临时用户用完即删。
- 审计日志为追加写、不可回收，会随每次运行累积（报告中会注明）。

---

## 各脚本说明（test/cases/）

| 脚本 | 测什么 | 预期结果 | 可能出现的情况/报错 |
|---|---|---|---|
| `00_connectivity.go` | 直连线上 PostgreSQL + Redis 并 PING | 两者均 PASS | PG/Redis 不可达（防火墙/白名单/密码错）→ FAIL，原因为连接错误文本 |
| `01_health_routes.go` | `/healthz` 与 hlt 版本 query + quota 路由可达性 | healthz 返回 `ok`；各业务路由返回 JSON 信封（非 404） | relay 未起来 → 连接错误；路由未注册 → 404 |
| `02_hlt_query.go` | 主接口 `POST querySrmxHLT` 全场景（个人健康评测，内部两步：授权备案→健康评测） | 成功 `errorCode=0/body.code=001`，`result.range` 为评测富对象 JSON（`hitCount`/`diseaseCategory`）；查无（无命中）`body.code=999`；错签 `505002`；未知 appKey `505004`；缺 appKey `505001`；手机号/身份证非法 `505062` | mock health(:9116) 未起 → 上游错误 `505062`；线上库异常 → 台账写入失败 `505062` |
| `04_found_count.go` | 成功查得数统计 + 无额度限制 | N 次成功 + M 次查无后 hlt `serviceUsed` 增量==N（查无不计） | 计数漂移（并发/复查）→ 增量≠N 时 FAIL |
| `06_admin_crud.go` | 管理后台全流程 | 登录(对/错)、建用户(返回 secret)、查/列、改(SUSPENDED)、轮换密钥(旧签失败/新签成功)、删、审计(过滤+PII 掩码)、无 token `401` | 登录失败 → 后续 JWT 步骤 SKIP |

> 说明：所有业务接口无论成功/失败均返回 HTTP 200，错误体现在信封里的 `head.errorCode`。

## 退出码

- 每个 case 脚本：有任意 FAIL → 退出码 1，否则 0（SKIP 不算失败）。
- `run.ps1`：任一脚本失败则整体退出码非 0，便于 CI 接入。
