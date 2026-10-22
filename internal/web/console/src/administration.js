// administration: task behavior and its event bindings.
import { state } from "./state.js";
import {
  $,
  api,
  esc,
  applyMutationPending,
  status,
  operationFeedback,
  clearOperationFeedback,
  beginMutation,
  endMutation,
  formData,
} from "./core.js";
import { applyAlertAvailability } from "./voices.js";
import { refreshOperationalStatus } from "./operations.js";

export async function loadGlobalAlerts() {
  if (!window.isAdmin) return;
  const v = await api("/api/v1/admin/alerts");
  $("#admin-alert-panel").classList.remove("hidden");
  $("#global-alerts-available").checked = !!v.available;
  applyAlertAvailability(v.available);
}

export async function loadUsers() {
  try {
    const rows = await api("/api/v1/users");
    $("#users-panel").classList.remove("hidden");
    $("#users").innerHTML = rows.length
      ? "<table><thead><tr><th>User</th><th>Role</th><th>Status</th><th></th></tr></thead><tbody>" +
        rows
          .map(
            (x) =>
              "<tr><td>" +
              esc(x.username) +
              "</td><td>" +
              esc(x.role) +
              "</td><td>" +
              (x.enabled ? "enabled" : "disabled") +
              "</td><td>" +
              (x.id === window.userId
                ? "current"
                : '<button class="link" data-delete-user="' +
                  esc(x.id) +
                  '">Delete</button>') +
              "</td></tr>",
          )
          .join("") +
        "</tbody></table>"
      : "<p>No users.</p>";
    applyMutationPending("#users");
  } catch (_) {
    $("#users-panel").classList.add("hidden");
  }
}

export async function loadSIP() {
  try {
    const v = await api("/api/v1/sip"),
      f = $("#sip-form");
    f.elements.enabled.checked = !!v.enabled;
    f.elements.domain.value = v.domain || "";
    f.elements.username.value = v.username || "";
    f.elements.local_port.value = v.local_port || v.port || 5060;
    f.elements.registrar_port.value = v.registrar_port || v.port || 5060;
    f.elements.transport.value = v.transport || "udp";
    f.elements.reg_interval.value =
      v.reg_interval == null ? 300 : v.reg_interval;
    f.elements.password.value = "";
    $("#sip-password-state").textContent = v.password_set
      ? "A password is stored. To change it, type a new one:"
      : "No password stored yet.";
    $("#sip-panel").classList.remove("hidden");
  } catch (_) {
    $("#sip-panel").classList.add("hidden");
  }
}

export async function loadVerification() {
  if (!window.isAdmin) return;
  try {
    const v = await api("/api/v1/admin/verification");
    $("#verification-incoming").checked = !!v.verification_incoming;
    $("#verification-outgoing").checked = !!v.verification_outgoing;
    $("#verification-dialin").value = v.verification_dialin || "";
    $("#verification-panel").classList.remove("hidden");
  } catch (_) {
    $("#verification-panel").classList.add("hidden");
  }
}

export async function loadAdminCallerPanel() {
  if (!window.isAdmin) return;
  try {
    const users = await api("/api/v1/users");
    const userSelect = $("#admin-phone-form")?.elements.user_id;
    if (userSelect) {
      userSelect.innerHTML = users
        .map(
          (u) =>
            '<option value="' + esc(u.id) + '">' + esc(u.username) + "</option>",
        )
        .join("");
    }
    $("#admin-caller-panel").classList.remove("hidden");
    if (userSelect) await loadAdminCallerAccounts(userSelect.value);
    await loadAdminCallerPhones();
  } catch (_) {
    $("#admin-caller-panel").classList.add("hidden");
  }
}

export async function loadAdminCallerAccounts(userID) {
  try {
    const data = await api("/api/v1/admin/whitelist?user_id=" + encodeURIComponent(userID));
    populateAdminAccountSelect(data.accounts || []);
    renderAdminPhones(data.whitelist || []);
  } catch (_) {
    $("#admin-caller-panel").classList.add("hidden");
  }
}

function populateAdminAccountSelect(accounts) {
  const select = $("#admin-phone-form")?.elements.account_id;
  if (!select) return;
  select.innerHTML = accounts
    .map(
      (a) =>
        '<option value="' + esc(a.id) + '">' + esc(a.canonical_name || a.name || a.id) + "</option>",
    )
    .join("");
}

export async function loadAdminCallerPhones() {
  const userID = $("#admin-phone-form")?.elements.user_id?.value;
  if (!userID) return;
  try {
    const data = await api("/api/v1/admin/whitelist?user_id=" + encodeURIComponent(userID));
    const select = $("#admin-phone-order-form")?.elements.phone;
    state.adminCallerPhones = data.whitelist || [];
    if (select) {
      select.innerHTML = state.adminCallerPhones
        .map(
          (x) =>
            '<option value="' + esc(x.phone) + '">' + esc(x.phone) + "</option>",
        )
        .join("");
      $("#admin-phone-order-form").classList.toggle("hidden", !state.adminCallerPhones.length);
      renderAdminOrder(state.adminCallerPhones[0]?.phone);
    }
  } catch (_) {}
}

function renderAdminPhones(rows) {
  $("#admin-phones").innerHTML = rows.length
    ? "<ul>" +
      rows
        .map((x) => {
          const names = (x.accounts || [])
            .map((a) => esc(a.account_name))
            .join(", ");
          return (
            "<li>" +
            esc(x.phone) +
            (x.verified ? "" : ' <span class="muted">unverified</span>') +
            (names ? " → " + names : "") +
            ' <button class="link" data-order-phone="' + esc(x.phone) + '">Reorder</button>' +
            ' <button class="link" data-delete-admin-phone="' + x.id + '">Remove</button>' +
            "</li>"
          );
        })
        .join("") +
      "</ul>"
    : "<p>No caller numbers for this user.</p>";
  applyMutationPending("#admin-phones");
}

function renderAdminOrder(phone) {
  const row = state.adminCallerPhones.find((x) => x.phone === phone);
  const list = $("#admin-phone-order");
  if (!row) {
    list.innerHTML = "";
    return;
  }
  list.innerHTML = (row.accounts || [])
    .map(
      (a, n) =>
        "<li>" + esc(a.account_name) +
        ' <span class="admin-order-buttons"><button class="link" data-order-up="' + esc(a.account_id) + '" aria-label="Move ' + esc(a.account_name) + ' up" title="Move up">↑</button>' +
        '<button class="link" data-order-down="' + esc(a.account_id) + '" aria-label="Move ' + esc(a.account_name) + ' down" title="Move down">↓</button></span></li>',
    )
    .join("");
  list.dataset.phone = phone;
}

export function initializeVerification() {
  $("#save-verification").onclick = async () => {
    const button = $("#save-verification");
    if (!beginMutation(button)) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback(button);
    try {
      await api("/api/v1/admin/verification", {
        method: "PUT",
        body: JSON.stringify({
          verification_incoming: $("#verification-incoming").checked,
          verification_outgoing: $("#verification-outgoing").checked,
          verification_dialin: $("#verification-dialin").value.trim(),
        }),
      });
      if (epoch !== state.sessionEpoch) return;
      status("Number verification settings saved.");
    } catch (x) {
      if (epoch === state.sessionEpoch)
        operationFeedback(button, "Verification settings could not be saved: " + x.message);
    } finally {
      if (epoch === state.sessionEpoch) endMutation(button);
    }
  };
}

export function initializeAdminCallerPanel() {
  $("#admin-phone-form")?.elements.user_id?.addEventListener("change", (e) => {
    loadAdminCallerAccounts(e.target.value).then(loadAdminCallerPhones);
  });
  $("#admin-phone-form").onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    if (!beginMutation(f, 'button[type="submit"]')) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback(f);
    try {
      await api("/api/v1/admin/whitelist", {
        method: "POST",
        body: JSON.stringify(formData(f)),
      });
      if (epoch !== state.sessionEpoch) return;
      f.elements.phone.value = "";
      await loadAdminCallerAccounts(f.elements.user_id.value);
      await loadAdminCallerPhones();
      status("Verified number added for that account.");
    } catch (x) {
      if (epoch === state.sessionEpoch)
        operationFeedback(f, "The number could not be added: " + x.message);
    } finally {
      if (epoch === state.sessionEpoch) endMutation(f, 'button[type="submit"]');
    }
  };
  $("#admin-phone-order-form").onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    if (!beginMutation(f, 'button[type="submit"]')) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback(f);
    try {
      const list = $("#admin-phone-order");
      const ids = [...list.querySelectorAll("li")].map(
        (li) => li.querySelector("[data-order-up]").dataset.orderUp,
      );
      await api("/api/v1/admin/whitelist/order", {
        method: "PUT",
        body: JSON.stringify({
          user_id: $("#admin-phone-form").elements.user_id.value,
          phone: list.dataset.phone,
          account_ids: ids,
        }),
      });
      if (epoch !== state.sessionEpoch) return;
      await loadAdminCallerAccounts($("#admin-phone-form").elements.user_id.value);
      status("Caller menu order saved.");
    } catch (x) {
      if (epoch === state.sessionEpoch)
        operationFeedback(f, "The order could not be saved: " + x.message);
    } finally {
      if (epoch === state.sessionEpoch) endMutation(f, 'button[type="submit"]');
    }
  };
  $("#admin-phone-order-form")?.elements.phone?.addEventListener("change", (e) =>
    renderAdminOrder(e.target.value),
  );
  $("#admin-phone-order-cancel").onclick = () =>
    renderAdminOrder($("#admin-phone-order-form").elements.phone.value);
  $("#admin-phones").onclick = async (e) => {
    const b = e.target.closest("button");
    if (!b) return;
    if (b.dataset.orderPhone) {
      $("#admin-phone-order-form").elements.phone.value = b.dataset.orderPhone;
      renderAdminOrder(b.dataset.orderPhone);
      $("#admin-phone-order-form").scrollIntoView({ behavior: "smooth" });
      return;
    }
    if (!b.dataset.deleteAdminPhone || !beginMutation("#admin-phones")) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback("#admin-phones");
    try {
      await api("/api/v1/whitelist/" + b.dataset.deleteAdminPhone, { method: "DELETE" });
      if (epoch !== state.sessionEpoch) return;
      await loadAdminCallerAccounts($("#admin-phone-form").elements.user_id.value);
      await loadAdminCallerPhones();
      status("Number removed.");
    } catch (x) {
      if (epoch === state.sessionEpoch)
        operationFeedback("#admin-phones", "The number could not be removed: " + x.message);
    } finally {
      if (epoch === state.sessionEpoch) endMutation("#admin-phones");
    }
  };
  $("#admin-phone-order").onclick = async (e) => {
    const b = e.target.closest("[data-order-up],[data-order-down]");
    if (!b || !beginMutation("#admin-phone-order")) return;
    try {
      const list = $("#admin-phone-order");
      const items = [...list.querySelectorAll("li")];
      const id = b.dataset.orderUp || b.dataset.orderDown;
      const i = items.findIndex((li) => li.querySelector("[data-order-up]").dataset.orderUp === id);
      const delta = b.dataset.orderUp ? -1 : 1;
      const j = i + delta;
      if (i < 0 || j < 0 || j >= items.length) return;
      list.insertBefore(items[j], delta < 0 ? items[i] : items[j].nextSibling);
      list.insertBefore(items[i], delta < 0 ? items[j] : null);
    } finally {
      endMutation("#admin-phone-order");
    }
  };
}

export function initializeAdministration() {
  initializeVerification();
  initializeAdminCallerPanel();
  $("#save-global-alerts").onclick = async () => {
    const button = $("#save-global-alerts");
    if (!beginMutation(button)) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback(button);
    try {
      const available = $("#global-alerts-available").checked;
      await api("/api/v1/admin/alerts", {
        method: "PUT",
        body: JSON.stringify({ available }),
      });
      if (epoch !== state.sessionEpoch) return;
      applyAlertAvailability(available);
      if (!available) $("#alert-numbers").innerHTML = "";
      status(
        available
          ? "Call alerts are available to users."
          : "Call alerts are disabled for all users.",
      );
    } catch (x) {
      if (epoch === state.sessionEpoch)
        operationFeedback(
          button,
          "Global alert setting could not be saved: " + x.message,
        );
    } finally {
      if (epoch === state.sessionEpoch) endMutation(button);
    }
  };
  $("#user-form").onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    if (!beginMutation(f, 'button[type="submit"]')) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback(f);
    try {
      await api("/api/v1/users", {
        method: "POST",
        body: JSON.stringify(formData(f)),
      });
      if (epoch !== state.sessionEpoch) return;
      f.reset();
      await loadUsers();
      status("User created.");
    } catch (x) {
      if (epoch === state.sessionEpoch)
        operationFeedback(f, "User could not be created: " + x.message);
    } finally {
      if (epoch === state.sessionEpoch) endMutation(f, 'button[type="submit"]');
    }
  };
  $("#users").onclick = async (e) => {
    const b = e.target.closest("[data-delete-user]");
    if (!b || !confirm("Delete this user?") || !beginMutation("#users")) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback("#users");
    try {
      const deletion = await api("/api/v1/users/" + b.dataset.deleteUser, {
        method: "DELETE",
      });
      if (epoch !== state.sessionEpoch) return;
      await loadUsers();
      status(
        deletion?.cleanup_pending
          ? "User deleted. Stored files are being removed in the background."
          : "User deleted.",
      );
    } catch (x) {
      if (epoch === state.sessionEpoch)
        operationFeedback("#users", "User removal failed: " + x.message);
    } finally {
      if (epoch === state.sessionEpoch) endMutation("#users");
    }
  };
  $("#sip-form").onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    if (!beginMutation(f, 'button[type="submit"]')) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback(f);
    try {
      const v = formData(f);
      v.enabled = f.elements.enabled.checked;
      v.local_port = Number(v.local_port || 5060);
      v.registrar_port = Number(v.registrar_port || 5060);
      v.reg_interval = Number(v.reg_interval || 300);
      await api("/api/v1/sip", { method: "PUT", body: JSON.stringify(v) });
      if (epoch !== state.sessionEpoch) return;
      status("SIP settings saved and applied to the call client.");
      await loadSIP();
      await refreshOperationalStatus();
    } catch (x) {
      if (epoch === state.sessionEpoch)
        operationFeedback(
          f,
          "SIP settings could not be saved or applied: " + x.message,
        );
    } finally {
      if (epoch === state.sessionEpoch) endMutation(f, 'button[type="submit"]');
    }
  };
}
