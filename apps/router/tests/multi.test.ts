import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Logger } from "../src/logger.ts";
import { AIHUB_ACCOUNT_HEADER, ManagedAccountRouter } from "../src/multi.ts";

let manager: ManagedAccountRouter | undefined;
let directory: string | undefined;

afterEach(async () => {
	await manager?.close();
	if (directory) await rm(directory, { recursive: true, force: true });
	manager = undefined;
	directory = undefined;
});

describe("managed AIHub accounts", () => {
	test("keeps the first account on the parent and starts an isolated child for the second", async () => {
		directory = await mkdtemp(join(tmpdir(), "aihub-auto-multi-"));
		manager = new ManagedAccountRouter(
			directory,
			process.execPath,
			join(import.meta.dir, "../src/main.ts"),
			{},
			new Logger("error", () => {}),
		);
		await manager.load();

		const first = new Request("http://localhost/ctl/account", {
			headers: { [AIHUB_ACCOUNT_HEADER]: "account-one" },
		});
		expect(await manager.routeControl(first, new URL(first.url))).toBeUndefined();

		const second = new Request("http://localhost/healthz", {
			headers: { [AIHUB_ACCOUNT_HEADER]: "account-two" },
		});
		const response = await manager.routeControl(second, new URL(second.url));
		expect(response?.status).toBe(200);
		expect(manager.list().map((account) => account.id)).toEqual([
			"account-one",
			"account-two",
		]);
	});
});
