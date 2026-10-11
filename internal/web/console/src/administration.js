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

export function initializeAdministration() {
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
