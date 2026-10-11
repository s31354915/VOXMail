// Layout follows the selected 12ui C conversion. All equivalence is owner-selected.
import { state } from "./state.js";
import { $, api, esc } from "./core.js";
import {
  folderPreviewModel,
  folderPreviewPage,
  remoteKey,
} from "./folder-preview.js";

const roleNames = {
  inbox: "Inbox",
  drafts: "Drafts",
  sent: "Sent",
  trash: "Trash",
  spam: "Spam",
  archive: "Archive",
};
const fixed = ["inbox", "drafts", "sent", "trash"];
let editor = null;
let opening = 0;
document.addEventListener("voxmail-session-cleared", () =>
  closeFolderEditor(false),
);

export function closeFolderEditor(focus = true) {
  opening++;
  const id = editor?.id;
  editor = null;
  $("#folder-editor").classList.add("hidden");
  $("#folder-editor").replaceChildren();
  if (focus && id)
    document
      .querySelector(`[data-folders-account="${CSS.escape(id)}"]`)
      ?.focus();
}

export async function openFolderEditor(account, afterSave) {
  const epoch = state.sessionEpoch,
    request = ++opening;
  const data = await api(
    `/api/v1/accounts/${encodeURIComponent(account.id)}/folder-policy`,
  );
  if (epoch !== state.sessionEpoch || request !== opening) return;
  editor = {
    id: account.id,
    name: account.canonical_name,
    epoch,
    version: data.config_version,
    folders: data.folders || {},
    mailboxes: data.mailboxes || [],
    bindings: data.folder_bindings || {},
    mapping: Object.assign(Object.create(null), data.folder_map),
    roles: { ...data.folder_roles },
    policy: structuredClone(data.policy || {}),
    selected: new Set(),
    active: "",
    busy: false,
    afterSave,
  };
  editor.policy.groups ||= [];
  editor.policy.excluded ||= [];
  renderEditor();
  $("#folder-editor").classList.remove("hidden");
  $("#folder-editor-title").focus();
  $("#folder-editor").scrollIntoView({ behavior: "smooth", block: "start" });
}

function message(text, error = false, focus = false) {
  const node = $("#folder-editor-status");
  node.textContent = text;
  node.classList.toggle("error", error);
  if (focus) node.focus();
}

function selectedGroup() {
  return editor.policy.groups.find((group) => group.id === editor.active);
}
function mappingName(remote) {
  return (
    Object.keys(editor.mapping).find(
      (name) => remoteKey(name) === remoteKey(remote),
    ) || remote
  );
}
function uniqueRemotes(remotes) {
  const seen = new Set();
  return remotes.filter((remote) => {
    const key = remoteKey(remote);
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}
function groupFor(remote) {
  return editor.policy.groups.find((group) =>
    group.folders.some((member) => remoteKey(member) === remoteKey(remote)),
  );
}
function excluded(remote) {
  return editor.policy.excluded.some(
    (member) => remoteKey(member) === remoteKey(remote),
  );
}
function names() {
  return Object.keys(editor.folders).sort(
    (a, b) => a.localeCompare(b) || (a < b ? -1 : 1),
  );
}
function option(value, label, selected) {
  return `<option value="${esc(value)}"${selected ? " selected" : ""}>${esc(label)}</option>`;
}
function labelFor(remote) {
  return groupFor(remote)?.name || remote;
}

function preview() {
  const model = folderPreviewModel(editor);
  const icons = [
    "added-083-envelope.svg",
    "cutout-91-3bccf74ae991.png",
    "added-091-paper-plane.svg",
    "added-095-trash-can.svg",
  ];
  const renderPage = (prefix = "", page = 0) => {
    const result = folderPreviewPage(model, prefix, page);
    let html = result.rows
      .map(({ key, entry, role }) => {
        const row = `<div class="mailbox" data-preview-folder="${esc(entry?.key || "")}" data-preview-key="${key}"><span class="number">${key}</span><img class="mailbox-icon" src="/assets/${role ? icons[fixed.indexOf(role)] : "added-099-folder.svg"}" alt="" width="22" height="22"><span>${esc(roleNames[role] || entry.name)}<small>${entry ? esc(entry.name) + (entry.folders.length > 1 ? ` · ${entry.folders.length} folders · Primary: ${esc(entry.primary || "choose a folder")}` : "") : "Not configured"}</small></span></div>`;
        const nested =
          entry && folderPreviewPage(model, entry.key).rows.length > 0;
        return (
          row +
          (nested
            ? `<details class="local-alias" data-preview-nested="${esc(entry.key)}"><summary>Nested folders in ${esc(entry.name)}</summary>${renderPage(entry.key)}</details>`
            : "")
        );
      })
      .join("");
    if (result.more)
      html += `<details class="local-alias" data-preview-more="${esc(prefix)}"><summary>Press 9 for more folders · page ${page + 2}</summary>${renderPage(prefix, page + 1)}</details>`;
    return html;
  };
  return renderPage();
}

function renderEditor() {
  const e = editor;
  if (!e || e.epoch !== state.sessionEpoch) return;
  const group = selectedGroup();
  $("#folder-editor").innerHTML = `
    <header class="editor-heading"><div><h2 id="folder-editor-title" tabindex="-1">Folder mapping for ${esc(e.name)}</h2><p>Choose folders, assign roles and decide which duplicates belong together. Choices take effect when saved.</p></div></header>
    <div class="editor-panels">
      <section class="panel"><div class="intro"><h3>Discovered folders</h3><p>Select folders for an action. Every ungrouped folder stays separate unless you choose otherwise.</p></div>
        <div class="folders">${
          names()
            .map((remote, index) => {
              const role =
                Object.keys(e.roles).find(
                  (role) => remoteKey(e.roles[role]) === remoteKey(remote),
                ) || "";
              return `<div class="folder-row">
            <input class="check" type="checkbox" data-folder-select="${esc(remote)}" id="folder-select-${index}" aria-label="Select ${esc(remote)}"${e.selected.has(remote) ? " checked" : ""}>
            <label for="folder-select-${index}">${esc(remote)}<small class="description">${excluded(remote) ? "Excluded" : groupFor(remote) ? `Group: ${esc(labelFor(remote))}` : "Separate"}</small></label>
            <div class="select-wrap"><select data-folder-role="${esc(remote)}" aria-label="Role for ${esc(remote)}">${option("", "No role", !role)}${Object.entries(
              roleNames,
            )
              .map(([key, name]) => option(key, name, role === key))
              .join("")}</select></div>
            <details class="local-alias"><summary>Local: ${esc(e.mapping[mappingName(remote)] ?? e.folders[remote])}</summary><label>Local folder<input data-folder-alias="${esc(remote)}" aria-label="Local folder for ${esc(remote)}" value="${esc(e.mapping[mappingName(remote)] ?? e.folders[remote])}"></label></details>
          </div>`;
            })
            .join("") ||
          "<p>No folder inventory yet. Use Test saved settings on this account to discover its provider folders, then reopen Folder choices.</p>"
        }</div>
        <div class="actions"><h3>Folder actions</h3><p id="folder-selection-count">${e.selected.size} selected</p><div class="action-buttons"><button type="button" data-folder-action="combine">Combine selected</button><button type="button" data-folder-action="separate">Keep separate</button><button type="button" data-folder-action="exclude">Exclude from browsing</button><button type="button" data-folder-action="include">Show selected</button></div><p class="description">Excluding a folder hides it from browsing. Its emails are retained.</p></div>
      </section>
      <section class="panel group"><div class="intro"><h3>Mailbox groups</h3><p>Choose a display name and primary folder. Messages from each selected member remain distinct.</p></div>
        <label class="group-choice">Edit group<select id="folder-group-choice">${option("", "Choose a group", !group)}${e.policy.groups.map((item) => option(item.id, item.name || "Unnamed group", item.id === e.active)).join("")}</select></label>
        <div class="group-box">${group ? `<label class="field-label" for="folder-group-name">Display name</label><input class="name-input" id="folder-group-name" value="${esc(group.name)}" maxlength="1024"><div class="members-heading"><span class="field-label">Members · choose primary</span></div><div class="member-list">${group.folders.map((remote, index) => `<div class="member"><input class="radio" type="radio" name="folder-primary" id="folder-primary-${index}" data-folder-primary="${esc(remote)}"${remoteKey(group.primary) === remoteKey(remote) ? " checked" : ""}><label for="folder-primary-${index}">${esc(remote)}</label>${remoteKey(group.primary) === remoteKey(remote) ? '<span class="primary">Primary</span>' : ""}</div>`).join("")}</div><div class="action-buttons"><button type="button" data-folder-action="add-members">Add selected members</button><button type="button" data-folder-action="dissolve">Keep all members separate</button></div>` : "<p>Select at least two folders and choose Combine selected to create a group. No primary is chosen automatically.</p>"}</div>
        <div class="roles"><h3>Folder roles</h3><p class="description">Assign each role once using the controls beside the discovered folders. A grouped role uses your chosen primary for writes.</p><div class="role-list">${Object.entries(
          roleNames,
        )
          .map(
            ([role, name]) =>
              `<div class="role-row"><strong>${esc(name)}</strong><span>${esc(e.roles[role] ? labelFor(e.roles[role]) : "Unassigned")}</span></div>`,
          )
          .join("")}</div></div>
      </section>
      <section class="panel result"><div class="intro"><h3>Resulting mailboxes</h3><p>Preview after the next successful sync. Inbox, Drafts, Sent and Trash keep keys 1–4. Other folders use 5–8; press 9 for more. Expand nested folders to see their keys.</p></div><div id="folder-preview" class="mailboxes">${preview()}</div><div class="notice"><div><h4>Excluded folders: ${e.policy.excluded.length}</h4><p>${esc(e.policy.excluded.join(", ") || "None. Every folder is retained in browsing.")}</p></div></div></section>
    </div>
    <footer class="editor-footer"><p id="folder-editor-status" class="editor-status" role="status" aria-live="polite" aria-atomic="true" tabindex="-1">Unsaved choices. Review the preview before saving.</p><div class="footer-buttons"><button class="button" type="button" data-folder-action="cancel">Cancel</button><button class="button save" type="button" data-folder-action="save">Save folder choices</button></div></footer>`;
  $("#folder-editor").onchange = onChange;
  $("#folder-editor").onclick = onClick;
}

function detach(remotes) {
  const keys = new Set([...remotes].map(remoteKey));
  for (const group of editor.policy.groups) {
    group.folders = group.folders.filter(
      (remote) => !keys.has(remoteKey(remote)),
    );
    if (
      !group.folders.some(
        (remote) => remoteKey(remote) === remoteKey(group.primary),
      )
    )
      group.primary = "";
  }
  editor.policy.groups = editor.policy.groups.filter(
    (group) => group.folders.length > 1,
  );
}

function onChange(event) {
  if (!editor || editor.busy || editor.epoch !== state.sessionEpoch) return;
  const input = event.target;
  if (input.hasAttribute("data-folder-select")) {
    const remote = input.dataset.folderSelect;
    input.checked
      ? editor.selected.add(remote)
      : editor.selected.delete(remote);
    $("#folder-selection-count").textContent =
      `${editor.selected.size} selected`;
    return;
  }
  if (input.hasAttribute("data-folder-alias")) {
    editor.mapping[mappingName(input.dataset.folderAlias)] = input.value;
    $("#folder-preview").innerHTML = preview();
    message("Local mapping changed. Save when ready.");
    return;
  }
  if (input.hasAttribute("data-folder-role")) {
    const remote = input.dataset.folderRole;
    for (const role of Object.keys(editor.roles))
      if (remoteKey(editor.roles[role]) === remoteKey(remote))
        delete editor.roles[role];
    if (input.value) {
      editor.roles[input.value] = remote;
      editor.mapping[mappingName(remote)] ??= editor.folders[remote];
    }
  }
  if (input.id === "folder-group-choice") editor.active = input.value;
  if (input.id === "folder-group-name") {
    selectedGroup().name = input.value;
    $("#folder-preview").innerHTML = preview();
    const selected = $("#folder-group-choice").selectedOptions[0];
    if (selected) selected.textContent = input.value || "Unnamed group";
    return;
  }
  if (input.hasAttribute("data-folder-primary"))
    selectedGroup().primary = input.dataset.folderPrimary;
  const refocus =
    input.id ||
    (input.dataset.folderRole
      ? `[data-folder-role="${CSS.escape(input.dataset.folderRole)}"]`
      : "");
  renderEditor();
  if (refocus)
    (refocus.startsWith("[")
      ? document.querySelector(refocus)
      : document.getElementById(refocus)
    )?.focus();
}

async function onClick(event) {
  const action = event.target.closest("[data-folder-action]")?.dataset
    .folderAction;
  if (!action || !editor || editor.busy || editor.epoch !== state.sessionEpoch)
    return;
  if (action === "cancel") {
    closeFolderEditor();
    return;
  }
  if (action === "save") {
    await save();
    return;
  }
  const selected = new Set(editor.selected),
    group = selectedGroup();
  if (action !== "dissolve" && selected.size === 0) {
    message("Select folders before choosing an action.", true, true);
    return;
  }
  if (action === "combine" && selected.size < 2) {
    message("Select at least two folders to combine.", true, true);
    return;
  }
  if (action === "add-members" && !group) {
    message("Choose a group first.", true, true);
    return;
  }
  if (action === "combine" || action === "separate" || action === "exclude")
    detach(selected);
  if (["combine", "separate", "include", "add-members"].includes(action))
    editor.policy.excluded = editor.policy.excluded.filter(
      (remote) =>
        ![...selected].some(
          (member) => remoteKey(member) === remoteKey(remote),
        ),
    );
  if (action === "combine") {
    const id = crypto.randomUUID();
    editor.policy.groups.push({
      id,
      name: "",
      primary: "",
      folders: [...selected],
    });
    editor.active = id;
  }
  if (action === "exclude")
    editor.policy.excluded = uniqueRemotes([
      ...editor.policy.excluded,
      ...selected,
    ]);
  if (action === "add-members") {
    const id = group.id;
    const original = { ...group };
    const members = uniqueRemotes([...group.folders, ...selected]);
    detach(selected);
    editor.policy.groups = editor.policy.groups.filter(
      (item) => item.id !== id,
    );
    editor.policy.groups.push({ ...original, folders: members });
    editor.active = id;
  }
  if (action === "dissolve" && group)
    editor.policy.groups = editor.policy.groups.filter(
      (item) => item.id !== group.id,
    );
  editor.selected.clear();
  renderEditor();
  if (action === "combine") $("#folder-group-name").focus();
  else {
    message("Choices updated. Review the preview, then save.", false, true);
  }
}

async function save() {
  const e = editor;
  const group = e.policy.groups.find(
    (item) => !item.name.trim() || !item.primary,
  );
  if (group) {
    message(
      "Each group needs a display name and an explicitly chosen primary folder.",
      true,
      true,
    );
    return;
  }
  e.busy = true;
  $("#folder-editor").setAttribute("aria-busy", "true");
  $("#folder-editor")
    .querySelectorAll("button,input,select")
    .forEach((input) => (input.disabled = true));
  message("Saving folder choices…");
  try {
    const result = await api(
      `/api/v1/accounts/${encodeURIComponent(e.id)}/folder-policy`,
      {
        method: "PUT",
        body: JSON.stringify({
          config_version: e.version,
          policy: e.policy,
          folder_map: e.mapping,
          folder_roles: e.roles,
        }),
      },
    );
    if (e !== editor || e.epoch !== state.sessionEpoch) return;
    e.version = result.config_version;
    message("Folder choices saved.", false, true);
    // Refresh failure must not make a successful save look retryable at the old version.
    try {
      await e.afterSave();
    } catch (error) {
      if (e === editor && e.epoch === state.sessionEpoch)
        message(
          "Folder choices saved, but the account list could not refresh: " +
            error.message,
          true,
          true,
        );
    }
  } catch (error) {
    if (e === editor && e.epoch === state.sessionEpoch)
      message(
        "Folder choices could not be saved: " + error.message,
        true,
        true,
      );
  } finally {
    if (e === editor && e.epoch === state.sessionEpoch) {
      e.busy = false;
      $("#folder-editor").setAttribute("aria-busy", "false");
      $("#folder-editor")
        .querySelectorAll("button,input,select")
        .forEach((input) => (input.disabled = false));
    }
  }
}
