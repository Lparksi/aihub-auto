import { join } from "node:path";
import type { Logger } from "./logger.ts";

export const AIHUB_ACCOUNT_HEADER = "x-aihub-account-id";
const ACCOUNT_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;
const MANIFEST_FILE = "accounts.json";
const FIRST_CHILD_PORT = 8788;

interface ManagedAccount {
	id: string;
	port: number;
	dir: string;
	createdAt: string;
}

interface AccountManifest {
	mainAccountId?: string;
	nextPort: number;
	accounts: Record<string, ManagedAccount>;
}

interface ChildRuntime {
	account: ManagedAccount;
	process: ReturnType<typeof Bun.spawn>;
}

export interface AccountView {
	id: string;
	port: number | null;
	main: boolean;
	createdAt: string | null;
}

function validAccountId(value: string): boolean {
	return ACCOUNT_ID_PATTERN.test(value);
}

function requestBodyAllowed(method: string): boolean {
	return method !== "GET" && method !== "HEAD";
}

/**
 * Owns isolated aihub-auto runtimes behind one public listener. The first
 * account keeps the existing process for backwards compatibility; later
 * accounts get their own config/state/credential directory and child port.
 */
export class ManagedAccountRouter {
	private manifest: AccountManifest = {
		nextPort: FIRST_CHILD_PORT,
		accounts: {},
	};
	private readonly children = new Map<string, ChildRuntime>();
	private mutation: Promise<unknown> = Promise.resolve();

	constructor(
		private readonly baseDir: string,
		private readonly executable: string,
		private readonly script: string | undefined,
		private readonly env: Record<string, string | undefined>,
		private readonly logger: Logger,
	) {}

	async load(): Promise<void> {
		try {
			const parsed = JSON.parse(
				await Bun.file(join(this.baseDir, MANIFEST_FILE)).text(),
			) as Partial<AccountManifest>;
			if (parsed && typeof parsed === "object") {
				this.manifest = {
					mainAccountId:
						typeof parsed.mainAccountId === "string" &&
						validAccountId(parsed.mainAccountId)
							? parsed.mainAccountId
							: undefined,
					nextPort:
						Number.isSafeInteger(parsed.nextPort) &&
						Number(parsed.nextPort) >= FIRST_CHILD_PORT
							? Number(parsed.nextPort)
							: FIRST_CHILD_PORT,
					accounts: Object.fromEntries(
						Object.entries(parsed.accounts ?? {}).filter(
							([id, account]) =>
								validAccountId(id) &&
								account &&
								Number.isSafeInteger(account.port) &&
								Number(account.port) > 0 &&
								typeof account.dir === "string",
						),
					),
				};
			}
		} catch {
			// A missing manifest is the normal first-run state.
		}
	}

	private persist(): Promise<void> {
		const text = JSON.stringify(this.manifest, null, 2);
		return Bun.write(join(this.baseDir, MANIFEST_FILE), text).then(() => undefined);
	}

	private serialize<T>(fn: () => Promise<T>): Promise<T> {
		const run = this.mutation.then(fn, fn);
		this.mutation = run.then(
			() => undefined,
			() => undefined,
		);
		return run;
	}

	private accountId(request: Request): string | undefined {
		const value = request.headers.get(AIHUB_ACCOUNT_HEADER)?.trim();
		return value && validAccountId(value) ? value : undefined;
	}

	private async allocatePort(): Promise<number> {
		let port = Math.max(this.manifest.nextPort, FIRST_CHILD_PORT);
		const used = new Set(
			Object.values(this.manifest.accounts).map((account) => account.port),
		);
		for (;;) {
			if (!used.has(port)) {
				try {
					const response = await fetch(`http://127.0.0.1:${port}/healthz`);
					await response.body?.cancel();
				} catch {
					this.manifest.nextPort = port + 1;
					return port;
				}
			}
			port++;
		}
	}

	private childCommand(port: number): string[] {
		const command = [this.executable];
		if (this.script) command.push(this.script);
		command.push("--port", String(port));
		return command;
	}

	private async startChild(account: ManagedAccount): Promise<ChildRuntime> {
		const childEnv: Record<string, string> = {};
		for (const [key, value] of Object.entries(this.env)) {
			if (value !== undefined) childEnv[key] = value;
		}
		childEnv.AIHUB_AUTO_CONFIG_DIR = account.dir;
		childEnv.AIHUB_AUTO_PORT = String(account.port);
		childEnv.AIHUB_AUTO_MULTI_CHILD = "1";
		const child = Bun.spawn(this.childCommand(account.port), {
			env: childEnv,
			stdout: "ignore",
			stderr: "ignore",
		});
		const runtime = { account, process: child };
		this.children.set(account.id, runtime);
		const deadline = Date.now() + 15_000;
		for (;;) {
			try {
				const response = await fetch(`http://127.0.0.1:${account.port}/healthz`);
				if (response.ok) return runtime;
			} catch {
				// Child is still starting.
			}
			if (Date.now() >= deadline) {
				this.children.delete(account.id);
				child.kill();
				throw new Error("aihub-auto 子账号 router 启动超时");
			}
			await Bun.sleep(100);
		}
	}

	private async resolve(accountId: string): Promise<ChildRuntime | undefined> {
		return this.serialize(async () => {
			if (this.manifest.mainAccountId === accountId) return undefined;
			if (!this.manifest.mainAccountId) {
				this.manifest.mainAccountId = accountId;
				await this.persist();
				this.logger.info(`AIHub 主账号绑定:${accountId}`);
				return undefined;
			}
			const existing = this.children.get(accountId);
			if (existing) return existing;
			let account = this.manifest.accounts[accountId];
			if (!account) {
				const port = await this.allocatePort();
				account = {
					id: accountId,
					port,
					dir: join(this.baseDir, "accounts", accountId),
					createdAt: new Date().toISOString(),
				};
				this.manifest.accounts[accountId] = account;
				await this.persist();
			}
			return this.startChild(account);
		});
	}

	private async forward(
		runtime: ChildRuntime,
		request: Request,
		url: URL,
	): Promise<Response> {
		const headers = new Headers(request.headers);
		headers.delete(AIHUB_ACCOUNT_HEADER);
		const body = requestBodyAllowed(request.method)
			? await request.clone().arrayBuffer()
			: undefined;
		return fetch(
			`http://127.0.0.1:${runtime.account.port}${url.pathname}${url.search}`,
			{
				method: request.method,
				headers,
				body: body && body.byteLength > 0 ? body : undefined,
			},
		);
	}

	async routeControl(request: Request, url: URL): Promise<Response | undefined> {
		const accountId = this.accountId(request);
		if (!accountId) return undefined;
		const runtime = await this.resolve(accountId);
		return runtime ? this.forward(runtime, request, url) : undefined;
	}

	async routeProxy(request: Request, url: URL): Promise<Response | undefined> {
		const accountId = this.accountId(request);
		if (!accountId) return undefined;
		const runtime = await this.resolve(accountId);
		return runtime ? this.forward(runtime, request, url) : undefined;
	}

	list(): AccountView[] {
		const result: AccountView[] = [];
		if (this.manifest.mainAccountId) {
			result.push({
				id: this.manifest.mainAccountId,
				port: null,
				main: true,
				createdAt: null,
			});
		}
		for (const account of Object.values(this.manifest.accounts)) {
			result.push({
				id: account.id,
				port: account.port,
				main: false,
				createdAt: account.createdAt,
			});
		}
		return result;
	}

	async close(): Promise<void> {
		for (const runtime of this.children.values()) runtime.process.kill("SIGTERM");
		this.children.clear();
	}
}
