// bootstrap: task behavior and its event bindings.
import { state } from "./state.js";
import { status, $, api } from "./core.js";
import {
  refreshOperationalStatus,
  scheduleOperationalPoll,
} from "./operations.js";
import { loadAccounts } from "./mailboxes.js";
import { loadContacts, loadPhones } from "./caller-access.js";
import {
  loadVoices,
  loadSettings,
  loadAlertNumbers,
  resumeActiveVoiceInstall,
} from "./voices.js";
import { loadGlobalAlerts, loadUsers, loadSIP } from "./administration.js";
import { renderNavigation } from "./navigation.js";
import { loadTwoFactor } from "./security.js";
import { renderAuthState, initialSetupState } from "./authentication.js";

export async function loadAll() {
  renderNavigation(false);
  const epoch = state.sessionEpoch;
  refreshOperationalStatus().then(() => {
    if (epoch === state.sessionEpoch && window.userId)
      scheduleOperationalPoll();
  });
  try {
    await loadTwoFactor();
    await loadSettings();
    await loadVoices();
    if (window.isAdmin) await loadGlobalAlerts();
    await Promise.all([
      loadAccounts(),
      loadContacts(),
      loadPhones(),
      state.alertsAvailable ? loadAlertNumbers() : Promise.resolve(),
      loadUsers(),
      window.isAdmin ? loadSIP() : Promise.resolve(),
    ]);
    if (window.isAdmin) void resumeActiveVoiceInstall();
  } catch (e) {
    status(e.message, true);
  }
}

export function initializeBootstrap() {
  (async () => {
    const flow = state.authEpoch;
    try {
      const u = await api("/api/v1/me");
      if (flow !== state.authEpoch) return;
      renderAuthState(false);
      window.userId = u.id;
      window.isAdmin = u.role === "admin";
      $("#auth").classList.add("hidden");
      $("#app").classList.remove("hidden");
      $("#who").textContent = u.username;
      await loadAll();
      return;
    } catch (e) {
      if (e.name === "AbortError" || flow !== state.authEpoch) return;
    }
    try {
      const info = await api("/api/v1");
      if (flow === state.authEpoch)
        renderAuthState(info.setup_available === true);
    } catch (e) {
      if (e.name !== "AbortError" && flow === state.authEpoch)
        renderAuthState(initialSetupState === "true");
    }
  })();
}
