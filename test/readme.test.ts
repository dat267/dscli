import { strict as assert } from "node:assert";
import { test } from "node:test";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

// The README's first help block must match the CLI's actual --help output.
test("README help block matches the CLI", () => {
	const output = execFileSync("node", ["--import", "tsx", join(root, "src", "cli.ts"), "--help"], {
		cwd: root,
		encoding: "utf8",
	});
	const readme = readFileSync(join(root, "README.md"), "utf8");
	const start = readme.indexOf("```\n");
	assert.ok(start >= 0, "README has no help block");
	const from = start + "```\n".length;
	const end = readme.indexOf("\n```", from);
	assert.ok(end >= 0, "README help block is not closed");
	assert.equal(readme.slice(from, end).trim(), output.trim());
});
