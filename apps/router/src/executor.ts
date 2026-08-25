import type { AIHubClient } from "@aihub-auto/core";
import { AIHubApiError } from "@aihub-auto/core";
import type { AppState, Credentials } from "./config.ts";
import type { Logger } from "./logger.ts";

export const POOL_KEY_PREFIX = "aihub-auto-g";
export const LUNA_POOL_KEY_PREFIX = "aihub-auto-luna-g";

export type ManagedPool = "default" | "luna";

export interface ActiveKey {
	sk: string;
	groupId: number;
	/** User-specific effective rate used by the same routing decision. */
	effectiveRate?: number;
	/** 请求开始计入 TrafficTracker 后释放 Key 逐出保护。 */
	release?: () => void;
	/** pool 模式上游拒绝当前 sk 时,仅在记录仍匹配时将其作废。 */
	invalidateCredential?: () => Promise<boolean>;
	/** 本次候选在首字节前失败时恢复原会话绑定。 */
	rollback?: () => void;
	/** 已提交响应随后断流时,仅清除仍属于本请求版本的绑定。 */
	invalidate?: () => void;
	/** 当前请求是否仍拥有会话主绑定。 */
	isCurrentBinding?: () => boolean;
}

export interface ExecutorDeps {
	client: AIHubClient;
	state: AppState;
	credentials: Credentials;
	logger: Logger;
	keyMode: "single" | "pool";
	singleKeyId?: number;
	poolMaxGroups: number;
	lunaPoolMaxGroups: number;
	evictionGraceMs?: number;
	/** 当前请求、预留与正在创建之外的硬保护组。 */
	hardProtectedGroupIds?: () => ReadonlySet<number>;
	/** 会话/Responses 亲和软保护;硬无效组可越过它回收。 */
	softProtectedGroupIds?: () => ReadonlySet<number>;
	/** 远端托管 Key 删除成功后的通知;仅通用池强制回收需要清掉亲和。 */
	onPoolKeyRemoved?: (groupId: number, forced: boolean, pool: ManagedPool) => void;
	persistState: () => Promise<void>;
	persistCredentials: () => Promise<void>;
	/** 401 时由 daemon 注入的续期回调;成功返回 true */
	reauth: () => Promise<boolean>;
}

/** AIHub 账号上的 Key 执行层。pool 请求只确保目标组 Key,不改变全局路由。 */
export class RouteExecutor {
	private readonly creating = new Map<string, Promise<ActiveKey>>();
	private readonly reservations = new Map<string, number>();
	private poolMutation: Promise<unknown> = Promise.resolve();

	constructor(private readonly deps: ExecutorDeps) {}

	private poolEntries(pool: ManagedPool): AppState["pool"] {
		return pool === "luna" ? this.deps.state.lunaPool : this.deps.state.pool;
	}

	private poolLimit(pool: ManagedPool): number {
		return pool === "luna"
			? this.deps.lunaPoolMaxGroups
			: this.deps.poolMaxGroups;
	}

	private scopedGroupId(pool: ManagedPool, groupId: number): string {
		return `${pool}:${groupId}`;
	}

	private keyName(pool: ManagedPool, groupId: number): string {
		return `${pool === "luna" ? LUNA_POOL_KEY_PREFIX : POOL_KEY_PREFIX}${groupId}`;
	}

	/** 控制面当前默认组对应的 Key。请求面应使用 ensureKey(groupId)。 */
	currentKey(): ActiveKey | undefined {
		const { state, credentials, keyMode } = this.deps;
		if (state.currentGroupId === undefined) return undefined;
		if (keyMode === "single") {
			if (!credentials.singleKeySk) return undefined;
			return { sk: credentials.singleKeySk, groupId: state.currentGroupId };
		}
		const entry = state.pool[String(state.currentGroupId)];
		if (!entry) return undefined;
		entry.lastUsedAt = Date.now();
		return { sk: entry.sk, groupId: state.currentGroupId };
	}

	private async withAuth<T>(fn: () => Promise<T>): Promise<T> {
		try {
			return await fn();
		} catch (err) {
			if (err instanceof AIHubApiError && err.status === 401) {
				const ok = await this.deps.reauth();
				if (ok) return await fn();
			}
			throw err;
		}
	}

	private serializePool<T>(fn: () => Promise<T>): Promise<T> {
		const run = this.poolMutation.then(fn, fn);
		this.poolMutation = run.then(
			() => undefined,
			() => undefined,
		);
		return run;
	}

	/** 请求面取得指定组 Key。single 模式因上游限制仍会全局切组。 */
	async ensureKey(
		groupId: number,
		pool: ManagedPool = "default",
	): Promise<ActiveKey> {
		if (this.deps.keyMode === "single") {
			const current = this.currentKey();
			return current?.groupId === groupId
				? current
				: this.switchSingle(groupId);
		}

		const scopedGroupId = this.scopedGroupId(pool, groupId);
		const entries = this.poolEntries(pool);
		const existing = this.creating.get(scopedGroupId);
		if (existing) return existing;

		// 逐出期间不得读取即将删除的缓存 Key;acquireKey 的 reservation 已经
		// 先可见,逐出会在远端删除前重新检查它。
		await this.poolMutation.catch(() => undefined);
		const cached = entries[String(groupId)];
		if (cached) {
			cached.lastUsedAt = Date.now();
			return { sk: cached.sk, groupId };
		}
		const afterWaitCreating = this.creating.get(scopedGroupId);
		if (afterWaitCreating) return afterWaitCreating;

		const pending = this.serializePool(async () => {
			const afterWait = entries[String(groupId)];
			if (afterWait) {
				afterWait.lastUsedAt = Date.now();
				return { sk: afterWait.sk, groupId };
			}

			const created = await this.withAuth(() =>
				this.deps.client.createKey({
					name: this.keyName(pool, groupId),
					groupId,
				}),
			);
			if (!created.key)
				throw new Error("创建 Key 未返回 sk 明文,无法用于池模式");

			entries[String(groupId)] = {
				keyId: created.id,
				sk: created.key,
				lastUsedAt: Date.now(),
			};
			this.deps.logger.info(
				`${pool === "luna" ? "Luna 池" : "池"}新建 Key:group=${groupId} keyId=${created.id}`,
			);
			await this.evictLru(pool, groupId);
			await this.deps.persistState();
			return { sk: created.key, groupId };
		});
		this.creating.set(scopedGroupId, pending);
		const clear = () => {
			if (this.creating.get(scopedGroupId) === pending)
				this.creating.delete(scopedGroupId);
		};
		void pending.then(clear, clear);
		return pending;
	}

	/** 请求面租约:TrafficTracker 接管保护前,Lru 不得删除这把 Key。 */
	async acquireKey(
		groupId: number,
		pool: ManagedPool = "default",
	): Promise<ActiveKey> {
		const scopedGroupId = this.scopedGroupId(pool, groupId);
		this.reservations.set(
			scopedGroupId,
			(this.reservations.get(scopedGroupId) ?? 0) + 1,
		);
		let released = false;
		const release = () => {
			if (released) return;
			released = true;
			const next = (this.reservations.get(scopedGroupId) ?? 1) - 1;
			if (next > 0) this.reservations.set(scopedGroupId, next);
			else this.reservations.delete(scopedGroupId);
		};
		try {
			const key = await this.ensureKey(groupId, pool);
			return {
				...key,
				release,
				invalidateCredential:
					this.deps.keyMode === "pool"
						? () => this.invalidatePoolKey(groupId, key.sk, pool)
						: undefined,
			};
		} catch (err) {
			release();
			throw err;
		}
	}

	/**
	 * 上游 401 说明请求用的 managed sk 已失效。expectedSk 是 CAS 保护,
	 * 防止旧请求删掉另一并发请求刚创建的新 Key。
	 */
	async invalidatePoolKey(
		groupId: number,
		expectedSk: string,
		pool: ManagedPool = "default",
	): Promise<boolean> {
		if (this.deps.keyMode !== "pool") return false;
		return this.serializePool(async () => {
			const entries = this.poolEntries(pool);
			const entry = entries[String(groupId)];
			if (!entry || entry.sk !== expectedSk) return false;
			delete entries[String(groupId)];
			this.deps.logger.warn(
				`${pool === "luna" ? "Luna 池" : "池"} Key 被上游拒绝,本地作废并重建:group=${groupId} keyId=${entry.keyId}`,
			);
			await this.deps.persistState();
			return true;
		});
	}

	/** 控制面切换默认组。pool 中只更新默认值,不会改动其他会话绑定。 */
	async switchTo(groupId: number): Promise<ActiveKey> {
		if (this.deps.keyMode === "single") return this.switchSingle(groupId);
		const key = await this.ensureKey(groupId);
		this.deps.state.currentGroupId = groupId;
		await this.deps.persistState();
		return key;
	}

	private async switchSingle(groupId: number): Promise<ActiveKey> {
		const { state, credentials, logger } = this.deps;
		let keyId = this.deps.singleKeyId;
		if (keyId === undefined || !credentials.singleKeySk) {
			const keys = await this.withAuth(() => this.deps.client.listAllKeys());
			const chosen =
				(keyId !== undefined
					? keys.find((key) => key.id === keyId)
					: undefined) ?? keys.find((key) => key.status !== "inactive");
			if (!chosen)
				throw new Error("账号下没有可用 API Key;请先在 AIHub 创建一个 Key");
			keyId = chosen.id;
			if (chosen.key) {
				credentials.singleKeySk = chosen.key;
				await this.deps.persistCredentials();
			} else if (!credentials.singleKeySk) {
				throw new Error(
					`Key 列表不返回 sk 明文;请在控制台把 Key(id=${keyId})的 sk 粘贴进配置(singleKeySk)`,
				);
			}
		}
		try {
			await this.withAuth(() =>
				this.deps.client.updateKeyGroup(keyId!, groupId),
			);
		} catch (err) {
			logger.warn(
				`切组 PUT 失败,重试一次: ${err instanceof Error ? err.message : String(err)}`,
			);
			await this.withAuth(() =>
				this.deps.client.updateKeyGroup(keyId!, groupId),
			);
		}
		state.currentGroupId = groupId;
		await this.deps.persistState();
		logger.info(`已切换(single):key=${keyId} -> group=${groupId}`);
		return { sk: credentials.singleKeySk!, groupId };
	}

	/** 定期收缩池;强无效组可越过会话软保护,删除成功后立即持久化。 */
	async trimPool(
		forceReclaimGroupIds: ReadonlySet<number> = new Set(),
	): Promise<number> {
		if (this.deps.keyMode !== "pool") return 0;
		return this.serializePool(async () => {
			const removed =
				(await this.evictLru("default", undefined, forceReclaimGroupIds)) +
				(await this.evictLru("luna", undefined, forceReclaimGroupIds));
			if (removed > 0) await this.deps.persistState();
			return removed;
		});
	}

	/** 超容量立即按 LRU 收缩无保护 Key;强无效组过宽限期后可越过软保护。 */
	private async evictLru(
		pool: ManagedPool,
		protectGroupId?: number,
		forceReclaimGroupIds: ReadonlySet<number> = new Set(),
	): Promise<number> {
		const { state, logger } = this.deps;
		const entries = this.poolEntries(pool);
		const isHardProtected = (groupId: number): boolean =>
			(protectGroupId !== undefined && groupId === protectGroupId) ||
			(pool === "default" && groupId === state.currentGroupId) ||
			this.creating.has(this.scopedGroupId(pool, groupId)) ||
			this.reservations.has(this.scopedGroupId(pool, groupId)) ||
			(this.deps.hardProtectedGroupIds?.().has(groupId) ?? false);
		const isSoftProtected = (groupId: number): boolean =>
			this.deps.softProtectedGroupIds?.().has(groupId) ?? false;
		const grace = this.deps.evictionGraceMs ?? 0;
		const now = Date.now();
		let removed = 0;
		const overCapacity = Object.keys(entries).length > this.poolLimit(pool);
		const victims = Object.entries(entries)
			.filter(([groupId, entry]) => {
				const id = Number(groupId);
				const forced = forceReclaimGroupIds.has(id);
				return (
					!isHardProtected(id) &&
					((forced && now - entry.lastUsedAt >= grace) ||
						(overCapacity && !isSoftProtected(id)))
				);
			})
			.sort((left, right) => left[1].lastUsedAt - right[1].lastUsedAt);

		while (victims.length > 0) {
			const [groupId, entry] = victims.shift()!;
			const id = Number(groupId);
			const forced = forceReclaimGroupIds.has(id);
			if (!forced && Object.keys(entries).length <= this.poolLimit(pool))
				break;
			// 快照之后可能出现创建/预留/在飞请求;删除前必须重新确认。
			if (isHardProtected(id) || (!forced && isSoftProtected(id))) continue;
			try {
				await this.withAuth(() => this.deps.client.deleteKey(entry.keyId));
				delete entries[groupId];
				this.deps.onPoolKeyRemoved?.(id, forced, pool);
				removed++;
				logger.info(
					`${pool === "luna" ? "Luna 池" : "池"}${forced ? "强制回收" : " LRU 删除"}:group=${groupId} keyId=${entry.keyId}`,
				);
			} catch (err) {
				logger.warn(
					`池删除失败(保留记录,下轮重试):keyId=${entry.keyId} ${err instanceof Error ? err.message : ""}`,
				);
				break;
			}
		}
		return removed;
	}

	/**
	 * 启动对账只清理本 state 的失效引用。未知远端前缀 Key 可能属于
	 * 同账号的另一个运行实例,绝不能作为“孤儿”自动删除。
	 */
	async reconcile(): Promise<void> {
		if (this.deps.keyMode !== "pool") return;
		await this.serializePool(async () => {
			const { logger } = this.deps;
			const keys = await this.withAuth(() => this.deps.client.listAllKeys());
			const remoteIds = new Set(keys.map((key) => key.id));

			for (const pool of ["default", "luna"] as const) {
				const entries = this.poolEntries(pool);
				for (const [groupId, entry] of Object.entries(entries)) {
					if (!remoteIds.has(entry.keyId)) {
						delete entries[groupId];
						logger.warn(
							`${pool === "luna" ? "Luna 池" : "池"}记录失效(远端已删):group=${groupId}`,
						);
					}
				}
			}
			await this.evictLru("default");
			await this.evictLru("luna");
			await this.deps.persistState();
		});
	}

	/** 退出清理(可选):删除全部自建 Key。 */
	async cleanup(): Promise<void> {
		await this.serializePool(async () => {
			const { logger } = this.deps;
			for (const pool of ["default", "luna"] as const) {
				const entries = this.poolEntries(pool);
				for (const [groupId, entry] of Object.entries(entries)) {
					try {
						await this.withAuth(() => this.deps.client.deleteKey(entry.keyId));
						delete entries[groupId];
					} catch (err) {
						logger.warn(
							`退出清理失败:keyId=${entry.keyId} ${err instanceof Error ? err.message : ""}`,
						);
					}
				}
			}
			await this.deps.persistState();
		});
	}
}
