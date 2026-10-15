// Layout follows the 12ui responsive folder-editor redesign. Grouping is owner-selected.
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
    warnings: data.warnings || [],
    folders: data.folders || {},
    mailboxes: data.mailboxes || [],
    bindings: data.folder_bindings || {},
    mapping: Object.assign(Object.create(null), data.folder_map),
    savedMapping: { ...data.folder_map },
    roles: { ...data.folder_roles },
    policy: structuredClone(data.policy || {}),
    selected: new Set(),
    active: "",
    busy: false,
    tab: "folders",
    query: "",
    page: 0,
    pageSize: window.matchMedia("(max-width: 600px)").matches ? 5 : 10,
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

function mappingOptions(remote) {
  const current = editor.mapping[mappingName(remote)] ?? editor.folders[remote];
  const choices = new Set([
    current,
    ...Object.values(roleNames),
    ...Object.values(editor.folders),
    ...Object.values(editor.mapping),
  ]);
  const localKey = (name) =>
    name
      .replaceAll("\\", "/")
      .split("/")
      .filter((part) => part && part !== ".")
      .join("/")
      .toLowerCase();
  const occupied = [
    ...Object.entries(editor.folders),
    ...Object.entries(editor.savedMapping),
    ...Object.entries(editor.mapping),
    ...Object.entries(editor.bindings).map(([local, owner]) => [owner, local]),
  ];
  // Offer a fresh destination even when all conventional names are occupied.
  // Keep existing and historical destinations reserved; grouping does not merge caches.
  const used = new Set(occupied.map(([, local]) => localKey(local)));
  let suggested = `Separate/${remote}`;
  for (let suffix = 2; used.has(localKey(suggested)); suffix++)
    suggested = `Separate/${remote}-${suffix}`;
  choices.add(suggested);
  return [...choices]
    .filter(Boolean)
    .sort((a, b) => a.localeCompare(b))
    .map((name) => {
      const taken = occupied.some(
        ([owner, local]) =>
          remoteKey(owner) !== remoteKey(remote) &&
          localKey(local) === localKey(name),
      );
      return `<option value="${esc(name)}"${name === current ? " selected" : ""}${taken && name !== current ? " disabled" : ""}>${esc(name)}${taken && name !== current ? " (in use — combine folders instead)" : ""}</option>`;
    })
    .join("");
}

function preview() {
  if (editor.warnings.length)
    return "<p>Resolve the folder mapping conflicts and save to refresh the phone preview.</p>";
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

function folderPage() {
  const all = names();
  const filtered = all.filter((remote) =>
    remote
      .toLocaleLowerCase()
      .includes(editor.query.toLocaleLowerCase().trim()),
  );
  const pages = Math.max(1, Math.ceil(filtered.length / editor.pageSize));
  editor.page = Math.min(editor.page, pages - 1);
  const start = editor.page * editor.pageSize;
  return {
    all,
    filtered,
    pages,
    start,
    rows: filtered.slice(start, start + editor.pageSize),
  };
}

function folderRow(remote, index) {
  const e = editor;
  const role =
    Object.keys(e.roles).find(
      (key) => remoteKey(e.roles[key]) === remoteKey(remote),
    ) || "";
  const local = e.mapping[mappingName(remote)] ?? e.folders[remote];
  return `<div class="folder-row${e.selected.has(remote) ? " is-selected" : ""}">
    <input class="check" type="checkbox" data-folder-select="${esc(remote)}" id="folder-select-${index}" aria-label="Select ${esc(remote)}"${e.selected.has(remote) ? " checked" : ""}>
    <label class="folder-name" for="folder-select-${index}"><img class="folder-icon" src="/assets/${role === "sent" ? "cutout-53-e8ef5973a543.png" : "added-099-folder.svg"}" alt="" width="20" height="20"><span>${esc(remote)}<small class="description">${excluded(remote) ? "Hidden from phone" : groupFor(remote) ? `Group: ${esc(labelFor(remote))}` : "Separate"}</small></span></label>
    <label class="select-wrap"><span class="field-caption">Role</span><select data-folder-role="${esc(remote)}" aria-label="Role for ${esc(remote)}">${option("", "No role", !role)}${Object.entries(
      roleNames,
    )
      .map(([key, name]) => option(key, name, role === key))
      .join("")}</select></label>
    <div class="folder-destination"><label><span class="field-caption">Destination</span><select data-folder-mapping="${esc(remote)}" aria-label="Map ${esc(remote)} to local folder">${mappingOptions(remote)}</select></label><details class="local-alias"><summary>Custom local name</summary><label>Local folder<input data-folder-alias="${esc(remote)}" aria-label="Local folder for ${esc(remote)}" value="${esc(local)}"></label></details></div>
    <label class="folder-visibility"><span class="field-caption">Phone visibility</span><select data-folder-visibility="${esc(remote)}" aria-label="Browsing for ${esc(remote)}">${option("show", "Show folder", !excluded(remote))}${option("ignore", "Hide from phone menu", excluded(remote))}</select></label>
  </div>`;
}

function renderEditor() {
  const e = editor;
  if (!e || e.epoch !== state.sessionEpoch) return;
  const group = selectedGroup();
  const page = folderPage();
  const tabs = [
    ["folders", "Folders", "added-099-folder.svg"],
    ["groups", "Groups", "folder-tabs-groups.svg"],
    ["preview", "Phone preview", "folder-tabs-phone.svg"],
  ];
  const panelAttrs = (tab) =>
    `id="folder-panel-${tab}" role="tabpanel" aria-labelledby="folder-tab-${tab}" tabindex="0"${e.tab === tab ? "" : " hidden"}`;
  $("#folder-editor").innerHTML = `
    <header class="editor-heading"><div><h2 id="folder-editor-title" tabindex="-1">Folder mapping for ${esc(e.name)}</h2><p>Choose folders, assign roles and decide which duplicates belong together. Choices take effect when saved.</p></div></header>
    <div class="editor-tabs" role="tablist" aria-label="Folder choices">${tabs.map(([tab, label, icon]) => `<button type="button" role="tab" id="folder-tab-${tab}" data-folder-tab="${tab}" aria-controls="folder-panel-${tab}" aria-selected="${e.tab === tab}" tabindex="${e.tab === tab ? 0 : -1}"><img src="/assets/${icon}" alt="" width="18" height="18">${label}</button>`).join("")}</div>
    ${e.warnings.length ? `<div class="notice" role="status"><div><h3>Folder mapping conflicts</h3>${e.warnings.map((warning) => `<p>${esc(warning)}</p>`).join("")}<p>Choose an unused destination from each folder’s dropdown, such as Separate/Sent. Existing destinations remain reserved to protect synced mail.</p></div></div>` : ""}
    <div class="editor-panels">
      <section class="panel folder-list-panel" ${panelAttrs("folders")}>
        <div class="intro"><h3>Discovered folders</h3><p>Select folders for an action. Selections stay selected across pages and searches.</p></div>
        <div class="folder-toolbar"><label class="folder-search"><span class="sr-only">Search folders</span><input type="search" id="folder-search" placeholder="Search folders…" value="${esc(e.query)}"></label><div class="action-buttons"><button type="button" data-folder-action="select-page">Select this page</button><button type="button" data-folder-action="clear-selection">Clear selection</button></div></div>
        <div class="folder-table-head" aria-hidden="true"><span></span><span>Folder</span><span>Role</span><span>Destination</span><span>Phone visibility</span></div>
        <div class="folders">${page.rows.map(folderRow).join("") || `<p class="folder-empty">${page.all.length ? "No folders match your search." : "No folder inventory yet. Use Test saved settings on this account to discover its provider folders, then reopen Folder choices."}</p>`}</div>
        <div class="folder-pagination"><label>Rows per page<select id="folder-page-size">${[5, 10, 25].map((size) => option(String(size), String(size), size === e.pageSize)).join("")}</select></label><span role="status" aria-live="polite">${page.filtered.length ? `${page.start + 1}–${Math.min(page.start + e.pageSize, page.filtered.length)}` : "0"} of ${page.filtered.length} folders</span><div class="page-buttons"><button type="button" data-folder-action="previous-page" aria-label="Previous page"${e.page === 0 ? " disabled" : ""}>‹</button><span>Page ${e.page + 1} of ${page.pages}</span><button type="button" data-folder-action="next-page" aria-label="Next page"${e.page + 1 >= page.pages ? " disabled" : ""}>›</button></div></div>
        <div class="actions"><div class="selection-summary"><h3>Folder actions</h3><p id="folder-selection-count">${e.selected.size} selected</p></div><div class="action-buttons"><button type="button" data-folder-action="combine">Combine selected</button><button type="button" data-folder-action="separate">Keep separate</button><button type="button" data-folder-action="exclude">Exclude from browsing</button><button type="button" data-folder-action="include">Show selected</button></div><p class="description">Excluded folders are hidden from the phone folder menu. Their emails are retained.</p></div>
      </section>
      <section class="panel group" ${panelAttrs("groups")}><div class="intro"><h3>Mailbox groups</h3><p>Select folders in the Folders tab, then combine them here. Choose a display name and a primary folder for writes.</p></div>
        <div class="group-layout"><div><label class="group-choice">Edit group<select id="folder-group-choice">${option("", "Choose a group", !group)}${e.policy.groups.map((item) => option(item.id, item.name || "Unnamed group", item.id === e.active)).join("")}</select></label>
        <div class="group-box">${group ? `<label class="field-label" for="folder-group-name">Display name</label><input class="name-input" id="folder-group-name" value="${esc(group.name)}" maxlength="1024"><div class="members-heading"><span class="field-label">Members · choose primary</span></div><div class="member-list">${group.folders.map((remote, index) => `<div class="member"><input class="radio" type="radio" name="folder-primary" id="folder-primary-${index}" data-folder-primary="${esc(remote)}"${remoteKey(group.primary) === remoteKey(remote) ? " checked" : ""}><label for="folder-primary-${index}">${esc(remote)}</label>${remoteKey(group.primary) === remoteKey(remote) ? '<span class="primary"><img src="/assets/cutout-162-3952d56441af.png" alt="" width="16" height="16">Primary</span>' : ""}</div>`).join("")}</div><div class="action-buttons"><button type="button" data-folder-action="add-members">Add selected members</button><button type="button" data-folder-action="dissolve">Keep all members separate</button></div>` : "<p>Select at least two folders in the Folders tab and choose Combine selected to create a group. No primary is chosen automatically.</p>"}</div></div>
        <div class="roles"><h3>Folder roles</h3><p class="description">Assign roles in the Folders tab. A grouped role uses your chosen primary for writes.</p><div class="role-list">${Object.entries(
          roleNames,
        )
          .map(
            ([role, name]) =>
              `<div class="role-row"><strong>${esc(name)}</strong><span>${esc(e.roles[role] ? labelFor(e.roles[role]) : "Unassigned")}</span></div>`,
          )
          .join("")}</div></div></div>
      </section>
      <section class="panel result" ${panelAttrs("preview")}><div class="intro"><h3>Resulting mailboxes</h3><p>Preview after the next successful sync. Inbox, Drafts, Sent and Trash keep keys 1–4. Other folders use 5–8; press 9 for more. Expand nested folders to see their keys.</p><p>On the phone, select a folder and press 1 to listen to messages there, or 2 to open its nested folders.</p></div><div id="folder-preview" class="mailboxes">${e.tab === "preview" ? preview() : ""}</div><div class="notice"><div><h4>Excluded folders: ${e.policy.excluded.length}</h4><p>${esc(e.policy.excluded.join(", ") || "None. Every folder is available in the phone menu.")}</p></div></div></section>
    </div>
    <footer class="editor-footer"><p id="folder-editor-status" class="editor-status" role="status" aria-live="polite" aria-atomic="true" tabindex="-1">Unsaved choices. Review the preview before saving.</p><div class="footer-buttons"><button class="button" type="button" data-folder-action="cancel">Cancel</button><button class="button save" type="button" data-folder-action="save">Save folder choices</button></div></footer>`;
  const root = $("#folder-editor");
  root.onchange = onChange;
  root.onclick = onClick;
  root.oninput = (event) => {
    if (event.target.id !== "folder-search" || e.busy) return;
    const caret = event.target.selectionStart;
    e.query = event.target.value;
    e.page = 0;
    renderEditor();
    $("#folder-search").focus();
    if (caret !== null) $("#folder-search").setSelectionRange(caret, caret);
  };
  root.onkeydown = (event) => {
    const tab = event.target.dataset.folderTab;
    if (!tab || e.busy) return;
    const keys = tabs.map(([id]) => id);
    let index = keys.indexOf(tab);
    if (event.key === "ArrowRight") index = (index + 1) % keys.length;
    else if (event.key === "ArrowLeft")
      index = (index + keys.length - 1) % keys.length;
    else if (event.key === "Home") index = 0;
    else if (event.key === "End") index = keys.length - 1;
    else return;
    event.preventDefault();
    e.tab = keys[index];
    renderEditor();
    document.getElementById(`folder-tab-${e.tab}`).focus();
  };
  root.setAttribute("aria-busy", String(e.busy));
  if (e.busy)
    root
      .querySelectorAll("button,input,select")
      .forEach((input) => (input.disabled = true));
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
  if (input.id === "folder-search") return;
  if (input.id === "folder-page-size") {
    editor.pageSize = Number(input.value);
    editor.page = 0;
    renderEditor();
    $("#folder-page-size").focus();
    return;
  }
  if (input.hasAttribute("data-folder-select")) {
    const remote = input.dataset.folderSelect;
    input.checked
      ? editor.selected.add(remote)
      : editor.selected.delete(remote);
    input.closest(".folder-row").classList.toggle("is-selected", input.checked);
    $("#folder-selection-count").textContent =
      `${editor.selected.size} selected`;
    return;
  }
  if (input.hasAttribute("data-folder-visibility")) {
    const remote = input.dataset.folderVisibility;
    editor.policy.excluded = editor.policy.excluded.filter(
      (member) => remoteKey(member) !== remoteKey(remote),
    );
    if (input.value === "ignore") {
      detach(new Set([remote]));
      editor.policy.excluded.push(remote);
    }
    renderEditor();
    document
      .querySelector(`[data-folder-visibility="${CSS.escape(remote)}"]`)
      ?.focus();
    return;
  }
  if (input.hasAttribute("data-folder-mapping")) {
    const remote = input.dataset.folderMapping;
    editor.mapping[mappingName(remote)] = input.value;
    renderEditor();
    document
      .querySelector(`[data-folder-mapping="${CSS.escape(remote)}"]`)
      ?.focus();
    message("Local mapping changed. Save when ready.");
    return;
  }
  if (input.hasAttribute("data-folder-alias")) {
    editor.mapping[mappingName(input.dataset.folderAlias)] = input.value;
    const select = document.querySelector(
      `[data-folder-mapping="${CSS.escape(input.dataset.folderAlias)}"]`,
    );
    select.innerHTML = mappingOptions(input.dataset.folderAlias);
    if (editor.tab === "preview") $("#folder-preview").innerHTML = preview();
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
    if (editor.tab === "preview") $("#folder-preview").innerHTML = preview();
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
  if (!editor || editor.busy || editor.epoch !== state.sessionEpoch) return;
  const tab = event.target.closest("[data-folder-tab]")?.dataset.folderTab;
  if (tab) {
    editor.tab = tab;
    renderEditor();
    document.getElementById(`folder-tab-${tab}`).focus();
    return;
  }
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
  if (
    ["next-page", "previous-page", "select-page", "clear-selection"].includes(
      action,
    )
  ) {
    const page = folderPage();
    if (action === "next-page")
      editor.page = Math.min(editor.page + 1, page.pages - 1);
    if (action === "previous-page") editor.page = Math.max(editor.page - 1, 0);
    if (action === "select-page")
      for (const remote of page.rows) editor.selected.add(remote);
    if (action === "clear-selection") editor.selected.clear();
    renderEditor();
    const control = document.querySelector(`[data-folder-action="${action}"]`);
    if (control && !control.disabled) control.focus();
    else $("#folder-panel-folders").focus();
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
    editor.tab = "groups";
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
    e.active = group.id;
    e.tab = "groups";
    renderEditor();
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
    Object.assign(e.savedMapping, e.mapping);
    message("Folder choices saved.", false, true);
    if (e.warnings.length) {
      try {
        const snapshot = await api(
          `/api/v1/accounts/${encodeURIComponent(e.id)}/folder-policy`,
        );
        if (e !== editor || e.epoch !== state.sessionEpoch) return;
        e.warnings = snapshot.warnings || [];
        e.folders = snapshot.folders || {};
        e.bindings = snapshot.folder_bindings || {};
        e.mailboxes = snapshot.mailboxes || [];
        renderEditor();
        message("Folder choices saved.", false, true);
      } catch (error) {
        if (e !== editor || e.epoch !== state.sessionEpoch) return;
        message(
          "Folder choices saved, but the preview could not refresh. Reopen Folder choices.",
          true,
          true,
        );
      }
    }
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
      const page = folderPage();
      $("[data-folder-action='previous-page']").disabled = e.page === 0;
      $("[data-folder-action='next-page']").disabled = e.page + 1 >= page.pages;
    }
  }
}
