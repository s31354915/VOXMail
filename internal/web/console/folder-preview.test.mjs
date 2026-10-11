import { readFile } from "node:fs/promises";
import test from "node:test";
import assert from "node:assert/strict";
import { folderPreviewModel, folderPreviewPage } from "./src/folder-preview.js";

const fixtures = JSON.parse(
  await readFile(
    new URL("../../../tests/fixtures/folder-menu.json", import.meta.url),
  ),
);
for (const fixture of fixtures) {
  test(fixture.name, () => {
    const model = folderPreviewModel(fixture.input);
    assert.deepEqual(
      model.entries.map((entry) => entry.key).sort(),
      [...fixture.folders].sort(),
    );
    const roles = Object.fromEntries(
      model.roleEntries.flatMap((entry, index) =>
        entry ? [[["inbox", "drafts", "sent", "trash"][index], entry.key]] : [],
      ),
    );
    assert.deepEqual(roles, fixture.roles);
    for (const page of fixture.pages) {
      const result = folderPreviewPage(model, page.prefix, page.page);
      assert.deepEqual(
        result.rows.map((row) => row.entry?.key || ""),
        page.keys,
      );
      assert.equal(result.more, page.more);
      assert.deepEqual(
        result.rows.map((row) => row.key),
        page.keys.map((_, index) => index + 1),
      );
    }
  });
}
