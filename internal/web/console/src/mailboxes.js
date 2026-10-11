// mailboxes: task behavior and its event bindings.
import { state } from "./state.js";
import { openFolderEditor, closeFolderEditor } from "./folder-editor.js";
import {
  $,
  formData,
  clearOperationFeedback,
  status,
  api,
  esc,
  operationFeedback,
  beginMutation,
  endMutation,
} from "./core.js";
import {
  syncDetails,
  refreshOperationalStatus,
  updateSyncButtons,
} from "./operations.js";

export function mapText(text) {
  const out = {};
  for (const line of String(text || "").split(/\r?\n/)) {
    const i = line.indexOf("=");
    if (i > 0) {
      const k = line.slice(0, i).trim(),
        v = line.slice(i + 1).trim();
      if (k && v) out[k] = v;
    }
  }
  return out;
}

export function mapTextBack(v) {
  return Object.entries(v || {})
    .map(([k, x]) => k + "=" + x)
    .join("\n");
}

export function beginAccountAction() {
  if (state.accountActionBusy) return false;
  state.accountActionBusy = true;
  $("#accounts").setAttribute("aria-busy", "true");
  $("#account-form").setAttribute("aria-busy", "true");
  document
    .querySelectorAll("#account-form button,#accounts button")
    .forEach((b) => (b.disabled = true));
  return true;
}

export function endAccountAction() {
  state.accountActionBusy = false;
  $("#accounts").setAttribute("aria-busy", "false");
  $("#account-form").setAttribute("aria-busy", "false");
  document
    .querySelectorAll("#account-form button,#accounts button")
    .forEach((b) => (b.disabled = false));
  updateSyncButtons();
}

export function accountPayload() {
  const f = $("#account-form"),
    v = formData(f),
    roles = {};
  for (const role of ["inbox", "sent", "drafts", "spam", "trash", "archive"])
    if (String(v["role_" + role] || "").trim())
      roles[role] = String(v["role_" + role]).trim();
  const a = {
    id: v.id || "",
    canonical_name: v.canonical_name,
    email: v.email,
    sender_name: v.sender_name,
    imap_host: v.imap_host,
    imap_port: Number(v.imap_port || 993),
    imap_security: v.imap_security,
    imap_user: v.imap_user,
    imap_password: v.imap_password,
    smtp_host: v.smtp_host,
    smtp_port: Number(v.smtp_port || 465),
    smtp_security: v.smtp_security,
    smtp_user: v.smtp_user,
    smtp_password: v.smtp_password,
    sync_interval_minutes: Number(v.sync_interval_minutes || 5),
    reconciliation_interval_minutes: Number(
      v.reconciliation_interval_minutes || 1440,
    ),
    display_order: Number(v.display_order || 0),
    folder_map: mapText(v.folder_map),
    folder_roles: roles,
    alert_folders: String(v.alert_folders || "")
      .split(",")
      .map((x) => x.trim())
      .filter(Boolean),
    call_alert_enabled: f.call_alert_enabled.checked,
  };
  if (v.config_version) a.config_version = Number(v.config_version);
  if (v.initial_cutoff) a.initial_cutoff = v.initial_cutoff;
  if (v.retention_days) a.retention_days = Number(v.retention_days);
  return a;
}

export function fillAccount(a) {
  clearOperationFeedback("#account-form");
  const f = $("#account-form");
  for (const k of [
    "id",
    "config_version",
    "canonical_name",
    "email",
    "sender_name",
    "imap_host",
    "imap_port",
    "imap_security",
    "imap_user",
    "smtp_host",
    "smtp_port",
    "smtp_security",
    "smtp_user",
    "sync_interval_minutes",
    "reconciliation_interval_minutes",
    "display_order",
    "initial_cutoff",
    "retention_days",
  ])
    if (f.elements[k]) f.elements[k].value = a[k] ?? "";
  f.elements.imap_password.value = "";
  f.elements.smtp_password.value = "";
  f.elements.folder_map.value = mapTextBack(a.folder_map);
  for (const role of ["inbox", "sent", "drafts", "spam", "trash", "archive"])
    f.elements["role_" + role].value = (a.folder_roles || {})[role] || "";
  f.elements.alert_folders.value = (a.alert_folders || []).join(", ");
  f.elements.call_alert_enabled.checked = !!a.call_alert_enabled;
  $("#test-saved-account").classList.remove("hidden");
  $("#account-test-help").textContent =
    "Test edited values uses this form without saving. Test saved settings ignores unsaved changes.";
  f.classList.remove("hidden");
  f.scrollIntoView({ behavior: "smooth", block: "start" });
  f.elements.canonical_name.focus();
}

export function resetAccount() {
  const f = $("#account-form");
  f.reset();
  f.elements.id.value = "";
  f.elements.imap_port.value = 993;
  f.elements.smtp_port.value = 465;
  f.elements.sync_interval_minutes.value = 5;
  f.elements.reconciliation_interval_minutes.value = 1440;
  $("#test-saved-account").classList.add("hidden");
  $("#account-test-help").textContent =
    "Tests the current form values without saving the account.";
  $("#folder-discovery").textContent = "";
  f.classList.add("hidden");
}

export async function saveAccount(a) {
  if (!a.id) {
    status("Testing the new mailbox values…");
    const result = await api("/api/v1/accounts/validate", {
      method: "POST",
      body: JSON.stringify(a),
    });
    showFolders(result.folders || []);
  }
  return api("/api/v1/accounts", { method: "POST", body: JSON.stringify(a) });
}

export function renderAccounts() {
  if (!state.accounts.length) {
    $("#accounts").innerHTML = "<p>No accounts configured.</p>";
    return;
  }
  const alertHead = state.alertsAvailable
    ? '<th class="alert-control">Alerts</th>'
    : "";
  $("#accounts").innerHTML =
    "<table><thead><tr><th>Order</th><th>Name</th><th>Address</th><th>IMAP</th><th>Sync</th>" +
    alertHead +
    "<th>Actions</th></tr></thead><tbody>" +
    state.accounts
      .map((a) => {
        const alert = state.alertsAvailable
          ? '<td class="alert-control" data-label="Alerts">' +
            (a.call_alert_enabled ? '<span class="pill">enabled</span>' : "—") +
            "</td>"
          : "";
        return (
          '<tr data-account-id="' +
          esc(a.id) +
          '"><td data-label="Order">' +
          esc(a.display_order) +
          '</td><td data-label="Name">' +
          esc(a.canonical_name) +
          '</td><td data-label="Address">' +
          esc(a.email) +
          '</td><td data-label="IMAP">' +
          esc(a.imap_host) +
          ":" +
          esc(a.imap_port) +
          '</td><td data-label="Sync" class="sync-detail" aria-live="polite" aria-atomic="true" data-sync-status="' +
          esc(a.id) +
          '">' +
          syncDetails(a) +
          "</td>" +
          alert +
          '<td data-label="Actions" class="actions"><button class="link" data-edit-account="' +
          esc(a.id) +
          '">Edit</button><button class="link" data-folders-account="' +
          esc(a.id) +
          '">Folder choices</button><button class="link" aria-label="Move ' +
          esc(a.canonical_name) +
          ' up" title="Move up" data-up-account="' +
          esc(a.id) +
          '">↑</button><button class="link" aria-label="Move ' +
          esc(a.canonical_name) +
          ' down" title="Move down" data-down-account="' +
          esc(a.id) +
          '">↓</button><button class="link" aria-label="Test saved settings for ' +
          esc(a.canonical_name) +
          '" data-test-account="' +
          esc(a.id) +
          '">Test saved settings</button><button class="link" data-sync-account="' +
          esc(a.id) +
          '">Sync</button><button class="link" data-delete-account="' +
          esc(a.id) +
          '">Delete</button></td></tr>'
        );
      })
      .join("") +
    "</tbody></table>";
  if (state.accountActionBusy)
    document
      .querySelectorAll("#accounts button:not([data-cancel-sync])")
      .forEach((button) => (button.disabled = true));
  updateSyncButtons();
}

export async function reorder(id, delta) {
  const i = state.accounts.findIndex((a) => a.id === id),
    j = i + delta;
  if (i < 0 || j < 0 || j >= state.accounts.length) return;
  const ids = state.accounts.map((a) => a.id);
  [ids[i], ids[j]] = [ids[j], ids[i]];
  await api("/api/v1/accounts/order", {
    method: "PUT",
    body: JSON.stringify({ ids }),
  });
  await loadAccounts();
  status("Mailbox order updated.");
}

export async function loadAccounts() {
  state.accounts = await api("/api/v1/accounts");
  renderAccounts();
}

export function showFolders(folders) {
  $("#folder-discovery").textContent =
    folders && folders.length
      ? "Discovered remote folders: " + folders.join(", ")
      : "No remote folders were returned.";
}

export function initializeMailboxes() {
  $("#new-account").onclick = () => {
    closeFolderEditor(false);
    clearOperationFeedback("#account-form");
    resetAccount();
    $("#account-form").classList.remove("hidden");
    $("#account-form").scrollIntoView({ behavior: "smooth" });
    $("#account-form").elements.canonical_name.focus();
  };
  $("#cancel-account").onclick = () => {
    resetAccount();
    $("#new-account").focus();
  };
  $("#account-form").onsubmit = async (e) => {
    e.preventDefault();
    if (!beginAccountAction()) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback("#account-form");
    try {
      const a = accountPayload();
      if (!a.id && (!a.imap_password || !a.smtp_password))
        throw Error("Both passwords are required for a new account");
      await saveAccount(a);
      if (epoch !== state.sessionEpoch) return;
      resetAccount();
      status("Account saved.");
      await loadAccounts();
    } catch (x) {
      if (epoch === state.sessionEpoch)
        operationFeedback(
          "#account-form",
          "Mailbox save or refresh failed: " + x.message,
        );
    } finally {
      if (epoch === state.sessionEpoch) endAccountAction();
    }
  };
  $("#test-account").onclick = async () => {
    if (!beginAccountAction()) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback("#account-form");
    try {
      const a = accountPayload(),
        result = await api("/api/v1/accounts/validate", {
          method: "POST",
          body: JSON.stringify(a),
        });
      if (epoch !== state.sessionEpoch) return;
      showFolders(result.folders || []);
      status(
        "Edited values authenticated; no changes were saved. " +
          (result.folders || []).length +
          " folders discovered.",
      );
    } catch (e) {
      if (epoch === state.sessionEpoch)
        operationFeedback(
          "#account-form",
          "Edited-value test failed: " + e.message,
        );
    } finally {
      if (epoch === state.sessionEpoch) endAccountAction();
    }
  };
  $("#test-saved-account").onclick = async () => {
    if (!beginAccountAction()) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback("#account-form");
    try {
      const id = $("#account-form").elements.id.value;
      if (!id) throw Error("Save the account before testing saved settings");
      const result = await api("/api/v1/accounts/test", {
        method: "POST",
        body: JSON.stringify({ account_id: id }),
      });
      if (epoch !== state.sessionEpoch) return;
      showFolders(result.folders || []);
      status(
        "Saved settings authenticated; unsaved edits were ignored. " +
          (result.folders || []).length +
          " folders discovered.",
      );
    } catch (e) {
      if (epoch === state.sessionEpoch)
        operationFeedback(
          "#account-form",
          "Saved-settings test failed: " + e.message,
        );
    } finally {
      if (epoch === state.sessionEpoch) endAccountAction();
    }
  };
  $("#accounts").onclick = async (e) => {
    const b = e.target.closest("button");
    if (!b) return;
    if (b.dataset.cancelSync) {
      if (!beginMutation(b)) return;
      const epoch = state.sessionEpoch;
      b.textContent = "Canceling…";
      clearOperationFeedback("#accounts");
      try {
        await api("/api/v1/accounts/" + b.dataset.cancelSync + "/sync/cancel", {
          method: "POST",
        });
        if (epoch !== state.sessionEpoch) return;
        status("Cancellation requested. Updating mailbox status…");
        await loadAccounts();
        await refreshOperationalStatus();
      } catch (x) {
        if (epoch === state.sessionEpoch) {
          b.textContent = "Cancel sync";
          operationFeedback(
            "#accounts",
            "Mailbox sync cancellation failed: " + x.message,
          );
        }
      } finally {
        if (epoch === state.sessionEpoch) endMutation(b);
      }
      return;
    }
    if (!beginAccountAction()) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback("#accounts");
    try {
      if (b.dataset.editAccount)
        fillAccount(state.accounts.find((a) => a.id === b.dataset.editAccount));
      if (b.dataset.foldersAccount)
        await openFolderEditor(
          state.accounts.find((a) => a.id === b.dataset.foldersAccount),
          loadAccounts,
        );
      if (
        b.dataset.deleteAccount &&
        confirm("Delete this account and its index?")
      ) {
        const deletion = await api(
          "/api/v1/accounts/" + b.dataset.deleteAccount,
          {
            method: "DELETE",
          },
        );
        if (epoch !== state.sessionEpoch) return;
        await loadAccounts();
        status(
          deletion?.cleanup_pending
            ? "Account deleted. Stored files are being removed in the background."
            : "Account deleted.",
        );
      }
      if (b.dataset.upAccount) await reorder(b.dataset.upAccount, -1);
      if (b.dataset.downAccount) await reorder(b.dataset.downAccount, 1);
      if (b.dataset.testAccount) {
        const result = await api("/api/v1/accounts/test", {
          method: "POST",
          body: JSON.stringify({ account_id: b.dataset.testAccount }),
        });
        if (epoch !== state.sessionEpoch) return;
        showFolders(result.folders || []);
        status(
          "Saved settings authenticated; " +
            (result.folders || []).length +
            " folders discovered.",
        );
      }
      if (b.dataset.syncAccount) {
        status("Synchronizing…");
        try {
          await api("/api/v1/accounts/" + b.dataset.syncAccount + "/sync", {
            method: "POST",
          });
          if (epoch === state.sessionEpoch)
            status(
              "Synchronization started. You can leave this page; progress and cancellation remain available.",
            );
        } finally {
          if (epoch === state.sessionEpoch) {
            await loadAccounts();
            await refreshOperationalStatus();
          }
        }
      }
    } catch (x) {
      if (epoch === state.sessionEpoch) {
        const id =
            b.dataset.deleteAccount ||
            b.dataset.testAccount ||
            b.dataset.syncAccount,
          name = state.accounts.find((a) => a.id === id)?.canonical_name;
        operationFeedback(
          "#accounts",
          (name ? name + ": " : "") + "Mailbox operation failed: " + x.message,
        );
      }
    } finally {
      if (epoch === state.sessionEpoch) endAccountAction();
    }
  };
}
