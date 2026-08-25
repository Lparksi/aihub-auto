import { describe, expect, test } from "bun:test";
import { applyManagedSecretOverrides, ConfigSchema, StateSchema } from "../src/config.ts";
import { matchesAccountPool } from "../src/daemon.ts";
import {
	applyStartupOptions,
	parseStartupOptions,
	STARTUP_HELP,
} from "../src/startup.ts";

describe("startup options", () => {
	test.each([
		[["--port", "9000"], {}, 9000],
		[["--port=9001"], {}, 9001],
		[[], { AIHUB_AUTO_PORT: "9002" }, 9002],
		[["--port", "9003"], { AIHUB_AUTO_PORT: "9004" }, 9003],
	] as const)("args=%j env=%j selects %i", (args, env, port) => {
		expect(parseStartupOptions([...args], env).port).toBe(port);
	});

	test.each(["0", "65536", "1.5", " 9000", "9000 ", "+9000", "09x"])(
		"rejects invalid environment port %s",
		(value) => {
			expect(() =>
				parseStartupOptions([], { AIHUB_AUTO_PORT: value }),
			).toThrow(/AIHUB_AUTO_PORT.*1.*65535/);
		},
	);

	test.each([
		["missing value", ["--port"]],
		["duplicate", ["--port=9000", "--port", "9001"]],
		["unknown", ["--listen", "9000"]],
	] as const)("rejects %s", (_name, args) => {
		expect(() => parseStartupOptions([...args], {})).toThrow();
	});

	test("help does not require a port and describes precedence", () => {
		expect(parseStartupOptions(["--help"], {}).help).toBe(true);
		expect(STARTUP_HELP).toContain("--port");
		expect(STARTUP_HELP).toContain("AIHUB_AUTO_PORT");
	});

	test("account pool plans are explicit unions", () => {
		const cases = [
			["A003-Plus", ["plus"], true],
			["A003-Pro", ["pro"], true],
			["A001-Team/K12", ["team"], true],
			["TEAM PLUS 混池", ["plus", "team"], true],
			["A003-Pro", ["plus", "team"], false],
			["A003-Plus", ["plus", "pro", "team"], true],
			["A003-Pro", ["plus", "pro", "team"], true],
			["A001-Team/K12", ["plus", "pro", "team"], true],
			["A008-BugTeam", ["team"], false],
			["任意分组", [], true],
			["A008-BugTeam", [], true],
		] as const;
		for (const [name, plans, expected] of cases) {
			expect(matchesAccountPool(name, plans, "all")).toBe(expected);
		}
		expect(matchesAccountPool("A003-Pro", [], "mixed")).toBe(true);
		expect(matchesAccountPool("A008-BugTeam", [], "mixed")).toBe(false);
		expect(ConfigSchema.parse({ accountPoolPlans: ["team", "plus"] }).accountPoolPlans).toEqual(["team", "plus"]);
	});

	test("旧状态自动补齐独立 Luna 池配置", () => {
		expect(ConfigSchema.parse({}).lunaPoolMaxGroups).toBe(4);
		expect(
			StateSchema.parse({
				pool: { "1": { keyId: 1, sk: "sk-old", lastUsedAt: 1 } },
			}).lunaPool,
		).toEqual({});
	});

	test("managed secrets override persisted router secrets", () => {
		const persisted = ConfigSchema.parse({
			uiPassword: "persisted-console-password",
			proxyToken: "persisted-proxy-token",
		});
		const effective = applyManagedSecretOverrides(persisted, {
			AIHUB_AUTO_UI_PASSWORD: "managed-console-password",
			AIHUB_AUTO_PROXY_TOKEN: "managed-proxy-token",
		});
		expect(effective.uiPassword).toBe("managed-console-password");
		expect(effective.proxyToken).toBe("managed-proxy-token");
		expect(persisted.uiPassword).toBe("persisted-console-password");
	});

	test("override changes memory but leaves the loaded object unchanged", () => {
		const loaded = ConfigSchema.parse({ listen: { port: 8123 } });
		const effective = applyStartupOptions(loaded, {
			help: false,
			port: 9123,
		});
		expect(effective.listen.port).toBe(9123);
		expect(loaded.listen.port).toBe(8123);
	});
});
