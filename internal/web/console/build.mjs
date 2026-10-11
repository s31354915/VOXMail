import { build } from "esbuild";
import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

const output = new URL("../static/console.bundle.js", import.meta.url);
const result = await build({
  entryPoints: [fileURLToPath(new URL("src/main.js", import.meta.url))],
  absWorkingDir: fileURLToPath(new URL("../../..", import.meta.url)),
  bundle: true,
  format: "iife",
  target: "es2020",
  legalComments: "none",
  write: false,
});
const bytes = result.outputFiles[0].contents;
if (process.argv.includes("--check")) {
  if (!Buffer.from(bytes).equals(await readFile(output))) {
    throw new Error("Console bundle is stale; run npm run build in internal/web/console.");
  }
} else {
  await writeFile(output, bytes);
}
