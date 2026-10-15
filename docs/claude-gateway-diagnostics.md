# Claude 网关兼容性与诊断

这组能力用于验证 Claude 请求形状、缓存断点与 SSE 完整性。诊断只观察，不决定计费、换号、重试或请求准入。

## SSE 诊断

在服务进程环境中设置：

```bash
SUB2API_CLAUDE_DIAGNOSTICS=true
```

原生 Anthropic Messages 流和 API-key 自动透传流会在现有日志中输出一条 `[ClaudeDiagnostics]` 摘要，包含事件计数、首事件/内容时间、上游 usage、是否读到 EOF 和固定原因码。它不记录文本、thinking、工具参数、凭据或上游自定义事件名称。现有响应字节、usage 计费和错误处理继续由原转发器负责。

- `clean`：本次读完的流没有触发诊断规则。
- `anomaly`：检测到明确矛盾，例如累计 output tokens 倒退、重复终止、已关闭 block 的 delta 或完整 EOF 缺少终止。
- `inconclusive`：未知事件、超出观测大小上限、不能解析或网关提前结束，无法完整判断。

未知事件不能说明上游恶意；协议更新也会产生新事件。input/cache usage 允许上游后续修订，不能按 output 的单调性规则判断。解析内存有上限：单行 8 KiB、单事件 32 KiB、最多 256 个同时打开的 block；超限只停止该部分诊断，不截断响应。

## 请求结构快照

已有开关现在默认写 NDJSON 结构摘要：

```bash
SUB2API_DEBUG_GATEWAY_BODY=/private/diagnostics/gateway_debug.log
```

摘要仅包含已知协议字段和 block 类型、缓存断点位置/TTL/数量、请求大小、system/tools 前 4 KiB 的 HMAC。header 只记录 `present`/`[redacted]`，不保存值。HMAC 密钥随机生成且不落盘，只适合在同一捕获实例内比较。未知字段名、类型名、工具名、metadata 值和上下文值不会写入。

文件权限为 `0600`，新建父目录为 `0700`，拒绝最终路径为符号链接或非普通文件的目标；父目录仍需由运维控制。已有文件会收紧权限并计入总大小预算。写入在单个文件达到 8 MiB 或实例运行一小时后停止；单条记录上限 64 KiB。达到上限不会自动轮转重开。文件保留供运维检查，需要按本地日志保留规则手动清理；这些上限不是自动删除期限。

只有另设以下开关才写请求原文（截取至单条预算的一部分）：

```bash
SUB2API_DEBUG_GATEWAY_RAW_BODY=true
```

原文可能包含用户消息、代码、工具结果和正文内的密钥。只在短期排障时开启，并在结束后清理捕获文件。普通结构摘要不需要该开关。

OAuth credential-scope 错误使用的旧 `ClaudeMimicDebug` 日志也已收紧为固定标签、存在性和大小：不再记录 URL、账号名称、system 预览、metadata 值或 header 值。这项保护不依赖诊断开关。

## API-key 上游策略

新策略需要同时满足进程开关和管理员 setting：

```bash
SUB2API_UPSTREAM_POLICY_V1_ENABLED=true
```

```json
{"upstream_policy_v1_enabled": true}
```

初次接入仅覆盖 `platform=anthropic,type=apikey`；OAuth、SetupToken、Bedrock/Mantle、Claude Platform on AWS、Vertex 和 OpenAI 仍使用原有路径。两个开关任一关闭，继续使用 legacy 账号字段。升级不会仅因数据库里留有实验配置就启用新策略。

启用后按 global override > 账号 > 分组 > 系统默认解析。透明 profile 使用 API-key 透传路径，并可保留客户端模型、UA、beta 和缓存前缀。必须的协议兼容清理（例如 count_tokens 的生成参数、非法 deferred-tool cache marker、缺少 beta 对应的字段）仍会执行；账号级 header overrides 仍是最终值。

账号 Extra 示例：

```json
{
  "upstream_passthrough": {
    "profile": "transparent",
    "overrides": {"forward_user_network_info": false}
  }
}
```

即使开启 permissive headers，也会剥离入站凭据、Cookie、hop-by-hop、Connection 指定的头和传输层自行生成的长度/压缩协商头。网络身份头通过独立开关复制一次，不会绕过 `forward_user_network_info=false`。上游认证由账号凭据注入。

请求每次换号前重新解析策略，避免旧账号的策略泄漏到下一个尝试。upstream 模型准入检查使用实际将发送的模型。计费仍遵循原有模型来源选择；如果透明别名没有价格，会保留账号配置的映射模型作为最终定价兜底，避免仅因跳过 wire rewrite 就失去价格。

## 管理诊断与协议回归

### 只读策略解释 API

完整管理员可通过现有管理认证调用：

```http
GET /api/v1/admin/accounts/{account_id}/upstream-policy
GET /api/v1/admin/accounts/{account_id}/upstream-policy?group_id={group_id}
```

可选分组必须真实存在且绑定该账号。不提供分组时，仅解释不带分组上下文的策略，不能代表所有用户分组的实际请求。接口不调用上游、不发测试请求、不修改账号或设置；沿用现有配置面权限边界，不向 `readonly_admin` 开放。

返回仅包含账号/分组数字 ID、固定枚举和布尔值，不包含账号名、URL、凭据、用户请求正文或任意 header 值：

- `gates`：进程 opt-in、数据库开关、账号资格和运行时是否启用；数据库读取失败为 `unavailable`，不冒充明确关闭。
- `runtime_mode`：`policy`、`legacy` 或 `ineligible`；未启用时不把新策略预设冒充旧路径的实际行为。
- `runtime_policy`：启用时的 profile/category 与每个 toggle 的值和来源。
- `candidate`：适用账号在当前配置下的新策略计算结果，可能尚未启用；不是实际流量观测或防封结论。

来源包括 category/group/account profile、system/account override、global override 和 legacy 字段。解释与真实请求使用同一 resolver；普通请求不会分配来源追踪 map。

### 代理探测的未知态

代理质量报告新增 `unknown_count`、`not_applicable_count`、`overall_status`，每项可带固定的 `evidence_source`/`reason_code`。探测源未配置、取消、无效响应或探测源 HTTP 错误保留“不确定”，不会直接认定坏代理；实际连接失败仍为失败，目标 429 仍为告警，Cloudflare challenge 仍单列。

未知项不计作失败，也不被算成健康。证据不完整时等级为 `unknown`，前端显示“证据不完整（未评级）”；兼容保留的数字分仅是已知项目的暂定分，不代表账号安全。未执行项为未知，不等同于不适用。

普通连接测试的 `status=unknown` 不覆盖最近的实测延迟；后台自动探测同样不把未知写成失败。出口地址族只来自已解析 IP，不推断双栈或 DNS 全路径；没有同地址族对照时不作跨地址族异常判断。没有新增第三方 IP 评分 API、账号冷却规则或网络探测目标。

### 版本化合成协议样本

`backend/internal/service/testdata/claude-wire-contract-v1/` 的 manifest 固定 schema 与来源；数据全部为 synthetic，版本标签不是某个实际 CLI 版本的抓包证明。回归测试通过真实 `Forward` 对照手写 expected body，并重放完整/截断 SSE，验证诊断开关不改变字节、usage 和错误结果。工具映射样本另检验只改协议名字字段、不改参数/普通文本；保留 Unicode、弯引号、日期及缓存断点。

```bash
cd backend
go test ./internal/service -run '^TestClaudeWireContract' -count=1
go test ./internal/service ./internal/handler/admin ./internal/server/routes -run 'Test(ExplainUpstream|GetUpstreamPolicy|UpstreamPolicyDiagnostics)' -count=1
```

这些样本用来发现协议兼容性回归，不用于删除地域标记、修改系统时区或证明封号因果。下一步可逆配置维护尚未实施，本轮没有引入系统清理或配置回滚功能。

## 协议修复与测试来源

- 对上游 429/5xx，Anthropic failover 耗尽后的默认 429/502/503 响应保留经现有校验器验证的 `Retry-After`；流已经输出时不再修改响应头。
- 工具名映射同步处理 `tool_choice`、`tool_use` 和原生 `tool_reference`，包括 tool-search 结果。反向恢复仅修改协议中的名字字段，不再全文替换工具参数和普通文本。
- 普通 400/404/413/422 不会因新增诊断而换号或冷却账号。没有从外部项目导入未经验证的重试阈值或错误规则。

实现独立编写，未引入新的运行时依赖或复制 AGPL/非商用源码。诊断设计参考 [Agent WireLens](https://github.com/Palind-Rome/agent-wirelens) 的有界捕获和 [API Relay Audit](https://github.com/toby-bridges/api-relay-audit) 的三态判定；协议回归方向参考 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)。原生工具引用形状按 [Anthropic tool-search 文档](https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool) 核对。

验证入口：

```bash
cd backend
go test -race ./internal/pkg/anthropicdiag -count=1
go test -tags unit -p 2 ./internal/service ./internal/handler -count=1
```
