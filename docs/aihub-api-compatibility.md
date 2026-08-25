# AIHub API 兼容性审查

> 审查日期：2026-08-20
>
> 范围：对 AIHub 公开接口进行匿名只读请求，并与本项目当前路由器实现比对。未使用账号凭据，也未执行任何会改变远端状态的请求。

## 结论

当前未发现会使路由器立即失效的公开 API 路径或核心字段冲突：公开分组统计和 Provider 统计的响应 envelope、路径及路由器依赖的延迟/可用性字段均可用。

AIHub 已在 Provider 统计中提供缓存命中率、成功率、模型健康和模型价格等新字段。项目现已解析缓存命中率与 5 分钟/6 小时成功率，并将其用于冷启动的保守路由；模型健康与模型价格仍待确认语义后再接入。

## 已验证兼容

### 公开分组统计

匿名请求：

```text
GET /api/v1/public/groups/usage-stats?platform=openai&samples=1
```

当前响应使用如下 envelope：

```json
{
  "code": 0,
  "data": {
    "items": [],
    "sample_limit": 1,
    "total": 0
  },
  "message": "..."
}
```

分组条目包含项目依赖的字段：

- `platform`
- `group_id`
- `rate_multiplier`
- `avg_ttft_ms`
- `sample_count`
- `last_sample_at`

项目请求和解析实现在 `packages/core/src/client.ts` 的 `getUsageStats()` 与 `parseGroupStat()` 中，当前兼容。

### Provider 统计

匿名请求：

```text
GET /api/v1/public/providers
```

当前 OpenAI Provider 条目仍提供项目核心评分需要的字段：

- `available`
- `group_id`
- `platform`
- `probe_e2e_ttft_ms`
- `probe_ttft_ms`
- `user_avg_ttft_ms`
- `user_sample_count`
- `user_has_data`

项目在 `packages/core/src/client.ts` 的 `getProviderLatencyStats()` 与 `parseProviderLatencyStat()` 中解析这些字段；它们继续支持官网探测 TTFT、真实用户 TTFT 和可用性硬过滤。

### 鉴权接口和模型 API 路径

以下匿名只读请求均返回 `401`，而不是 `404`：

```text
GET /api/v1/auth/me
GET /api/v1/groups/available
GET /api/v1/groups/rates
GET /api/v1/keys?page=1&page_size=1
GET /v1/models
```

这确认当前管理 API 与 OpenAI API 路径仍存在，且鉴权边界符合项目预期。路由器的上游代理目标为 `/v1/...`，实现在 `apps/router/src/proxy.ts` 的 `handleProxyRequest()` 中。

## 已利用的云端数据

### Provider `cache_hit_rate`

审查时，全部 OpenAI Provider 条目均包含字符串形式的缓存命中率，例如：

```json
"cache_hit_rate": "88.03%"
```

项目会将百分比解析为 `0..1` 的数值，`"-"`/空值视为缺失。当请求带有稳定提示前缀或 `prompt_cache_key`、但还没有已有会话绑定时，路由器会按当前模式的延迟权重加入受限缓存效用（最大 `0.25 × latencyWeight`），而不是把命中率当作确定命中。已有会话仍优先回到原分组，不会因渠道指标被迁移。

项目会从实际模型响应中提取 `cached_tokens`，并将单个会话标记为缓存命中或未命中：

- 提取：`apps/router/src/proxy.ts` 的 `findCacheUsage()`
- 持久化会话状态：`apps/router/src/session.ts` 的 `recordCache()`
- 使用缓存状态判断会话是否可能仍热：`apps/router/src/session.ts` 的 `cacheLikelyHot()`

这与 AIHub 发布的渠道级缓存命中率不同。当前实现同时使用两者：本地命中结果维护已有会话亲和，云端聚合命中率辅助冷启动选择。

### Provider `success_rates`

当前 Provider 条目提供多时间窗口的成功率，例如：

```json
{
  "5m": 1,
  "6h": 0.921875,
  "24h": 0.8784461152882206,
  "7d": 0.7757630704140224,
  "30d": 0.6654339250493096
}
```

项目仍以本地近三小时的代理结果、熔断器状态，以及 `available: false` 的硬过滤为主：

- 评分过滤：`packages/core/src/scoring.ts`
- 本地观测：`packages/core/src/observe.ts`
- 熔断：`packages/core/src/breaker.ts`

AIHub 的 `5m` 成功率（缺失时回退 `6h`）现作为本地结果置信度不足时的低权重失败率先验，最多只贡献 25% 权重，并会随着本地结果置信度提高衰减到 0。它只增加保守延迟，不替代本地熔断或直接排除候选。

## 仍待确认的功能缺口

### `model_health`、`model_detection` 与 `model_prices` 未被使用

当前 Provider 条目还包含：

- `model_health`
- `model_detection`
- `model_prices`

项目路由以分组 `rate_multiplier` 为成本输入，并主要通过请求失败后的本地缓存学习“组 × 模型”不兼容；见 `packages/core/src/scoring.ts` 和 `apps/router/src/proxy.ts` 的 `isModelIncompatibleResponse()`。

因此，公开的按模型健康状态、检测状态和缓存输入价格尚未参与：

- 预先排除不健康的“组 × 模型”组合；
- 基于指定模型的成本排序；
- 基于缓存输入单价的成本排序。

这些是尚未利用的新 API 能力，不是已证实的请求或响应协议冲突。

## 未验证范围

匿名请求只能验证 API 路径存在及未认证时的行为，不能验证成功响应结构。下列接口仍需在获得用户明确授权并使用测试账号的前提下，做最小权限的只读或可逆验证：

- 登录和刷新 Token：`/api/v1/auth/login`、`/api/v1/auth/refresh`
- 可用分组和用户专属倍率：`/api/v1/groups/available`、`/api/v1/groups/rates`
- API Key 列表和创建/改组/删除：`/api/v1/keys`

对 Key 创建、切组或删除接口的验证会产生远端状态变更，不能作为普通兼容性探测执行。

## 建议的后续工作

1. 确认 `model_health`、`model_detection` 与请求模型名之间的映射语义；映射可靠后，为“组 × 模型”候选增加预过滤。
2. 确认 `model_prices` 是否为用户实际计费价格及其与 `rate_multiplier` 的关系；确认后再决定是否扩展 economy 策略。
3. 继续保留并扩展固定响应 fixture，防止 Provider 响应演进时静默忽略关键字段。
