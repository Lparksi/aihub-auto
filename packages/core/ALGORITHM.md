# @aihub-auto/core 算法说明

AIHub 当前只提供 OpenAI 分组。路由器分成两个互不冲突的平面:

- **控制面**:定时读取公开统计,维护可解释评分与默认组
- **请求面**:仅在新会话或故障转移时做 P2C/Peak-EWMA 分配;已有会话保持原组

数据源为 `GET {base}/api/v1/public/groups/usage-stats?platform=openai&samples=...` 的真实请求兼容统计、`GET {base}/api/v1/public/providers` 的官网真实用户均值与标准化云端探测、账号可用组/专属倍率,以及代理真实流量的本地观测。

## 1. 硬过滤

候选必须同时满足:

| 条件 | `excludeReason` |
| --- | --- |
| OpenAI 平台 | `platform_mismatch` |
| 官网 provider 未明确标记 `available=false`,且属于账号可用组(成功取得账号列表时) | `unavailable_group` |
| 生效倍率有限、非负且位于 `[priceBand.min, priceBand.max]` | `invalid_rate` / `price_band` |
| 不在用户黑名单 | `blacklisted` |
| 不处于熔断冷却 | `circuit_open` |
| 上游或本地至少一方有可用 TTFT | `invalid_latency` |
| 无高置信度本地高错误率 | `local_error_rate` |

用户专属倍率优先于公开倍率。上游接口返回的最新有效样本始终可用,不会因年龄标记为过期。

## 2. 上游 TTFT 先验

官网 providers 提供两路不同口径:

- `user_avg_ttft_ms`:真实用户平均 TTFT,`user_has_data=false` 或非正数时不参与
- `probe_e2e_ttft_ms`:标准化云端探测端到端 TTFT,缺失时回退 `probe_ttft_ms`

旧 `usage-stats.avg_ttft_ms` 与官网用户均值同属真实请求统计,仅在新用户字段缺失时作为兼容回退,绝不把两者重复计权。有效上游证据的 `publicConfidence = 1`;`last_sample_at` 年龄和样本数量只展示,不用于排除或降权。

## 3. 本地观测与 Peak EWMA

TTFT 与最终成败分开记录:

- 首个响应字节:`recordLatency(group, ttft)`
- 流正常结束:`recordSuccess(group)`
- 首字节前路由故障或可观测的中途断流:`recordFailure(group)`
- 客户端主动取消不算上游失败

每组保留最近 20 个 TTFT 用于 P90/CV,最终成功/失败则保留最近 3 小时、最多 500 条。普通 EWMA 的 `alpha=0.3`;Peak EWMA 对峰值立即响应,低值只按时间缓慢恢复:

```text
if ttft > peak:
  peak = ttft
else:
  w = exp(-ln2 * elapsed / 5min)
  peak = peak * w + ttft * (1 - w)
```

错误率/成功率来自该 3 小时最终结果窗口;P90、CV 和延迟置信度仍来自 TTFT 短窗口。本地置信度为:

```text
localConfidence = (1 - exp(-recentSamples / 3)) * exp(-ln2 * age / 5min)
```

## 4. 三源 TTFT 融合

先把官网真实用户均值与云端探测视为两路独立上游证据,在对数空间等权融合;只有一路时原值直通。然后把本地风险延迟按 `localConfidence` 在同一对数空间双向融合。本地风险延迟优先使用 Peak EWMA,没有时才使用 `0.7 * EWMA + 0.3 * P90`;本地样本数为 0 时严格不参与。

```text
upstreamTtft = geometricMean(available(userAvgTtft, cloudProbeTtft))
localWeight = localTtftSamples > 0 ? localConfidence : 0
blendedTtft = upstreamValid
  ? exp((1 - localWeight) * ln(upstreamTtft) + localWeight * ln(localRiskLatency))
  : localRiskLatency
effectiveError = min(0.95, localErrorRate * outcomeConfidence)
conservativeLatency = blendedTtft * (2 - confidence) / max(1 - effectiveError, 0.2)
```

几何融合与后续对数效用同尺度,不会把同一份用户数据算两次;任一路缺失或为 0 都不占权重。本地更快会下调延迟,本地更慢也会抬高延迟。高错误率仍由稳定率门槛、熔断和硬过滤处理。

## 5. 价格与速度得分

`economy` 先应用显式健康门槛:已有最近结果且成功率为 0% 时立即淘汰;达到 `minOutcomeSamples` 后成功率还必须不低于 `minSuccessRate`,保守 TTFT 必须不超过 `maxConservativeLatencyMs`。默认值为 3 条、80%、20 秒,均可在控制台调整。

随后从健康候选中计算 `minimumRate`,当前路由层只包含 `effectiveRate === minimumRate` 的最低健康价格层。更高倍率的健康候选进入 `standby`,它们**可用但当前不花这笔钱**,不属于排除项;最低层因本次失败、熔断、模型不兼容或健康门槛全部退出后,下一价格层自动提升为当前层。当前层内部按保守延迟和在飞负载选择。

三种模式共享尺度无关的对数效用。有效倍率均为正时:

```text
minimumRate  = min(candidate.effectiveRate)
fastest      = min(candidate.conservativeLatency)
pricePenalty = ln(effectiveRate / minimumRate)
latencyGain  = ln(fastest / conservativeLatency)
score        = latencyWeight * latencyGain - priceWeight * pricePenalty
```

因此统一放大所有倍率或延迟不会改变排序,添加无关候选也不会反转已有候选之间的相对优势。`balanced` 的 0.5/0.5 表示倍率增加 `k` 倍必须换来延迟降低 `k` 倍才持平;`speed` 的 0.2/0.8 允许用更多倍率换取延迟。`minimumRate=0` 时仍只允许零倍率候选按对数延迟竞争,非零倍率为 `-Infinity`;不引入隐藏 epsilon 价格。

| 模式 | 价格策略 | 延迟权重 |
| --- | --- | ---: |
| `economy` | 最低健康有效倍率硬约束 | 0.2(同价层,常数权重不改变排序) |
| `balanced` | 对数价格 0.5 权重 | 0.5 |
| `speed` | 对数价格 0.2 权重 | 0.8 |

排序顺序为 score 降序、生效倍率升序、保守延迟升序、groupId 升序。

## 6. 控制面默认组

`decide()` 仍用于控制台、审计和默认组预热。正常优化门槛为:

```text
trafficRecency = activeStreams > 0 ? 1 : clamp01(1 - idle / cacheIdleMs)
threshold = stickiness + cachePenaltyMax * trafficRecency
```

它提供 `hold_sticky`、`hold_cache`、`pending_realized`、`minDwell` 等防抖语义。对数得分下 `advantage=0.1` 表示约 10.5% 的复合乘法效用提升;默认热缓存门槛 `0.1+0.25=0.35` 表示约 42% 提升。等倍率时这对应约 5.75 倍(economy)、2.01 倍(balanced)、1.55 倍(speed)的延迟改善,体现各模式本身的优先级,而不是隐藏的模式阈值。pool 模式下改变默认组不会迁移已绑定会话;single 兼容模式受上游单 Key 限制,代理流生命周期与控制面物理切组共享 FIFO 租约,不会在长流中途改组。

## 7. 请求面:会话亲和 + P2C

会话标识按以下优先级生成不可逆 SHA-256 摘要,并始终按模型隔离:

1. `x-aihub-auto-session`
2. Responses `conversation`
3. `previous_response_id` 响应别名;未知 ID 也有稳定回退摘要
4. `prompt_cache_key`
5. 模型、instructions、tools 与稳定提示前缀

已有绑定只检查平台、账号可用性、倍率区间、黑名单、样本健康、模型能力和熔断等硬约束,不会因新评分或 `economy` 最低价层变化而迁移。`previous_response_id` 会解析为 `sessionKey + preferredGroupId`(响应别名记录的实际上游组);该分支优先回到原组,但**不改写**会话主绑定,避免并发分支互相覆盖。

控制台手动锁定持久化为带 revision 的运行状态。显式会话、conversation、Responses 分支和仍热的缓存亲和优先;其余新会话/无状态请求优先锁定组。锁定可覆盖 economy 的稳定率/延迟软门槛,但不能绕过账号不可用、倍率区间、黑名单、无效延迟、硬错误率、模型能力、熔断或当前请求已失败组。锁组故障时仅本请求临时逃生,锁定意图不被清除。

请求调度从当前模式排序后的有限候选中直接取前 `poolMaxGroups` 个,不复用 Koishi 展示专用的 `scoreWindow`。静态最优组始终参加比较,另一个挑战者由会话摘要稳定抽取;空载不会随机牺牲质量,积压后仍可使用池内全部容量。负载效用与静态评分使用同一公式:

```text
loadedLatency = conservativeLatency * (activeByGroup + pendingByGroup + 1)
loadedScore   = score - latencyWeight * ln(activeByGroup + pendingByGroup + 1)
```

当请求带有 `prompt_cache_key` 或稳定提示前缀等缓存线索时,再加入渠道级 `cache_hit_rate` 的先验效用:

```text
cacheBonus = latencyWeight * 0.25 * clamp01(cache_hit_rate)
loadedScore += cacheBonus
```

其中 `0.25` 是最大缓存效用上限,会随 `economy`/`balanced`/`speed` 的延迟权重缩放;没有缓存线索或接口返回 `"-"` 时加分为零。它只影响新请求的冷启动挑战者选择,已有会话仍按本地缓存命中/未命中和会话亲和回到原组。

负载惩罚不再收敛到固定下限,所以任意有限价格候选在相对积压足够高时都能接管流量。这里的计数是连续负载量,同一个分组可以同时承担多个请求,不会被当成单槽资源。等权候选用会话哈希拆分。`economy` 候选仍严格限制在当前最低价层,负载不会单独触发加价;`balanced`/`speed` 按各自权重比较。会话绑定后不做动态迁移。

## 8. 故障、熔断与模型能力

以下错误记为组健康失败并允许**首字节前**请求本地重试:连接错误、TTFB 超时(含“响应头已回但 body 首字节卡住”)、429、5xx。并发同会话请求用版本化绑定:旧请求失败回滚/迟到缓存证据不得覆盖较新主绑定。每个失败组每次尝试只记一次:

- 连续失败 3 次,或 10 秒窗口至少 4 个样本且失败率达到 50%:closed -> open
- 冷却为 `30s * 2^(openCount-1)`,上限 10 分钟
- 冷却后只允许一个请求面 half-open 探针;成功关闭,失败重新打开

普通 4xx 原样透传,不影响组健康。只有强模型能力信号才写入带 TTL 的 `(hash(model), groupId)` 负缓存:

- 结构化 code:`model_not_found`、`unsupported_model`、`model_not_supported`
- sub2api/Codex 确定性 `model is not supported when using codex` 响应
- 同等明确的中英文“不支持/未知模型”消息

模型不兼容只排除该 group/model,不惩罚该组的其他模型。故障转移通过会话锁与 CAS rebind 只更新失败会话;其他热会话不动。响应开始后绝不透明重放,避免重复输出和重复计费。

## 9. 自动 Key 池

pool 是默认模式。`ensureKey(groupId)` 使用同组 single-flight,不同组的提交/逐出串行化。

会话与 Responses 路由映射保留到 `sessionTtlMs`(默认 24 小时),但只有最近 `decision.cacheIdleMs`(默认 5 分钟)使用过的亲和组会短期保护 Key。缓存窗口结束后,普通 LRU 可回收 Key 而不删除会话映射;后续续接时按原组自动重建 Key,因此连续性不占用长期池容量。

普通 LRU 在超过 `poolMaxGroups` 时立即回收没有近期缓存亲和的最旧 Key;缓存窗口只保护真实近期亲和,不为无亲和 Key 提供额外宽限。手动锁定 Key 受普通 LRU 软保护,但硬无效强回收仍可删除坏 Key而保留锁定意图。倍率区间外、用户黑名单、账号不可用、延迟无效、近 3 小时稳定率过低,以及最新成功统计里已消失的历史组属于强无效原因,经过缓存宽限后可越过旧亲和回收。`standby` 升档层是健康可用组,绝不因此强制回收;熔断冷却是临时状态,也只参与路由排除和普通 LRU。

每次远端删除前都会重新检查硬保护:控制面当前组、正在创建、路由预留和在飞请求永远不删。只有强无效 Key 删除成功后才同步清掉该组会话与 Responses 分支亲和。LRU 在创建新 Key、启动对账和每轮守护时执行;若没有安全删除对象,池允许暂时超过上限。未被回收的池状态持久化并在重启后对账复用。启动对账只清理本 state 指向但远端已不存在的记录;未知远端 `aihub-auto-g*` Key 可能属于同账号另一实例,绝不自动删除。模型上游以 401 拒绝 managed Key 时,代理按 `groupId + expectedSk` 原子作废旧记录、同组重建并在首字节前重试,旧并发请求不能删掉已刷新的 Key。`/ctl/status` 只返回 `keyId/lastUsedAt`,不返回 `sk`。

## 10. Koishi topN

```text
按 score 降序,保留 score >= best - scoreWindow(默认 0.15),最多 6 条
```

Koishi 同时使用公开 usage-stats 与 providers 三源字段,但不参与请求面会话路由;其 `economy` 推荐同样只展示最低有效倍率层。

## 11. 默认参数

| 参数 | 默认值 |
| --- | ---: |
| `priceBand` | `[0, 0.15]` |
| `errorRateCap` | 0.5 |
| `stickiness` / `cachePenaltyMax` | 0.10 / 0.25 |
| `cacheIdleMs` / `minDwellMs` | 5 分钟 / 90 秒 |
| `scoreWindow` / Koishi max | 0.15 / 6 |
| EWMA alpha / TTFT 窗口 / 结果窗口 | 0.3 / 20 / 3 小时(最多 500) |
| 会话 TTL / 上限 | 24 小时 / 10000 |
| economy 稳定率 / 样本门槛 / 最大保守 TTFT | 80% / 3 / 20 秒 |
| pool 目标组数 | 4 |
