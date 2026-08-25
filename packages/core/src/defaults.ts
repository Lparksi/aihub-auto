import type {
	DecisionPolicy,
	EconomyPolicy,
	ModeWeights,
	RoutingMode,
} from "./types.ts";

export const MODE_WEIGHTS: Record<RoutingMode, ModeWeights> = {
	economy: { priceWeight: 0.8, latencyWeight: 0.2 },
	balanced: { priceWeight: 0.5, latencyWeight: 0.5 },
	speed: { priceWeight: 0.2, latencyWeight: 0.8 },
};

export const DEFAULT_PRICE_BAND = { min: 0, max: 0.15 };

export const DEFAULT_ECONOMY_POLICY: EconomyPolicy = {
	minOutcomeSamples: 3,
	minSuccessRate: 0.8,
	maxConservativeLatencyMs: 20_000,
};

/** 置信度下限,低于淘汰 */
export const MIN_CONFIDENCE = 0.2;

/** 本地观测错误率淘汰阈值 */
export const DEFAULT_ERROR_RATE_CAP = 0.5;

/** topN 推荐:与最优分差窗口 / 最大条数 */
export const DEFAULT_SCORE_WINDOW = 0.15;
export const DEFAULT_TOPN_MAX = 6;

export const DEFAULT_DECISION_POLICY: DecisionPolicy = {
	stickiness: 0.1,
	cachePenaltyMax: 0.25,
	/** OpenAI 默认内存缓存通常在 5-10 分钟无活动后清理 */
	cacheIdleMs: 5 * 60_000,
	minDwellMs: 90_000,
};

/**
 * 缓存命中对请求效用的最大贡献(对数效用单位)。
 * 命中率只作为冷启动偏好，已有会话仍由会话亲和保证，不会被重新排序迁移。
 */
export const DEFAULT_CACHE_UTILITY_MAX = 0.25;

/** 本地观测 EWMA 系数 */
export const LOCAL_EWMA_ALPHA = 0.3;
/** 本地 TTFT 短窗口大小(P90/CV)。 */
export const LOCAL_WINDOW_SIZE = 20;
/** 稳定率使用最近 3 小时结果,并限制样本数避免高流量无限增长。 */
export const LOCAL_OUTCOME_WINDOW_MS = 3 * 60 * 60_000;
export const LOCAL_OUTCOME_MAX_SAMPLES = 500;
/** 本地延迟置信度半衰期 */
export const LOCAL_CONFIDENCE_HALF_LIFE_MS = 5 * 60_000;

/** 熔断器默认参数 */
export const BREAKER_DEFAULTS = {
	consecutiveFailureThreshold: 3,
	windowMs: 10_000,
	windowFailureRate: 0.5,
	windowMinSamples: 4,
	baseCooldownMs: 30_000,
	maxCooldownMs: 10 * 60_000,
};
