// core: task behavior and its event bindings.
import { state } from "./state.js";
import { setAuthMode } from "./authentication.js";
import { clearSecurityErrors, clearTwoFactor } from "./security.js";

export const $ = (s) => document.querySelector(s);

export const esc = (v) =>
  String(v ?? "").replace(
    /[<>&"']/g,
    (c) =>
      ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );

export const status = (msg, error = false) => {
  if (!window.userId || !msg) return;
  const output = $("#app-status");
  output.textContent = msg;
  output.className = "status" + (error ? " error" : "");
  if (!msg.endsWith("…")) output.focus();
};

export function operationFeedback(target, message, error = true) {
  if (!window.userId) return;
  const element = typeof target === "string" ? $(target) : target;
  if (!element) return status(message, error);
  const id = element.getAttribute("id") + "-operation-feedback";
  let output = document.getElementById(id);
  if (!output) {
    output = document.createElement("p");
    output.id = id;
    output.className = "status operation-feedback";
    output.setAttribute("role", "status");
    output.setAttribute("aria-live", "polite");
    output.setAttribute("aria-atomic", "true");
    output.tabIndex = -1;
    element.insertAdjacentElement("afterend", output);
  }
  output.textContent = message;
  output.classList.toggle("error", error);
  output.focus();
}

export function clearOperationFeedback(target) {
  const element = typeof target === "string" ? $(target) : target;
  if (!element) return;
  const output = document.getElementById(
    element.getAttribute("id") + "-operation-feedback",
  );
  if (output) {
    output.textContent = "";
    output.classList.remove("error");
  }
}

export function setMutationPending(target, pending, selector = "button") {
  const element = typeof target === "string" ? $(target) : target;
  if (!element) return;
  if (pending && element.dataset.mutationPending === "true") return false;
  element.dataset.mutationPending = String(pending);
  element.setAttribute("aria-busy", String(pending));
  if (element.matches("button")) element.disabled = pending;
  element
    .querySelectorAll(selector)
    .forEach((button) => (button.disabled = pending));
  return true;
}

export function beginMutation(target, selector = "button") {
  return setMutationPending(target, true, selector);
}

export function endMutation(target, selector = "button") {
  setMutationPending(target, false, selector);
}

export function applyMutationPending(target, selector = "button") {
  const element = typeof target === "string" ? $(target) : target;
  if (element?.dataset.mutationPending === "true") {
    if (element.matches("button")) element.disabled = true;
    element
      .querySelectorAll(selector)
      .forEach((button) => (button.disabled = true));
  }
}

export function clearSensitiveFields() {
  document
    .querySelectorAll(
      "input[type=password],input[name=pin],input[name=totp],input[name=backup_code],input[name=code],input[name=secret]",
    )
    .forEach((x) => (x.value = ""));
}

export function changedSession() {
  const e = new Error("");
  e.name = "AbortError";
  return e;
}

export function beginSession() {
  state.sessionEpoch++;
  state.sessionAbort.abort();
  state.sessionAbort = new AbortController();
  clearTimeout(state.operationalTimer);
  state.operationalTimer = null;
  state.operationalBusy = false;
  state.operationalRequest = null;
}

export function clearSessionView() {
  document.dispatchEvent(new Event("voxmail-session-cleared"));
  state.accounts = [];
  state.contacts = [];
  state.alertsAvailable = true;
  window.userId = "";
  window.isAdmin = false;
  document.body.dataset.signedIn = "false";
  delete document.body.dataset.consoleTask;
  state.syncSeenStates.clear();
  $("#admin-nav-art").classList.add("hidden");
  document.title = "VOXMail Console";
  $("#admin-navigation").classList.add("hidden");
  clearTwoFactor();
  clearSecurityErrors();
  [
    "#accounts",
    "#contacts",
    "#phones",
    "#users",
    "#alert-numbers",
    "#voice-list",
    "#twofa-status",
    "#password-status",
    "#pin-status",
    "#app-status",
    "#voice-status",
    "#sip-password-state",
    "#folder-discovery",
    "#who",
    "#refresh-status-announcement",
    "#sync-change-announcement",
    "#voice-announcement",
    "#voice-progress",
    "#sip-failure",
    "#operational-status",
    "#operational-updated",
    "#sync-freshness",
  ].forEach((s) => {
    const e = $(s);
    if (e) e.textContent = "";
  });
  document.querySelectorAll('[data-mutation-pending="true"]').forEach((el) => {
    el.dataset.mutationPending = "false";
    el.setAttribute("aria-busy", "false");
    if (el.matches("button")) el.disabled = false;
    el.querySelectorAll("button").forEach(
      (button) => (button.disabled = false),
    );
  });
  [
    "#setup",
    "#login",
    "#recovery-request",
    "#recovery-confirm",
    "#account-form",
    "#contact-form",
    "#phone-form",
    "#settings-form",
    "#alert-number-form",
    "#user-form",
    "#sip-form",
    "#password-form",
    "#pin-form",
    "#twofa-form",
  ].forEach((s) => {
    const f = $(s);
    if (f) f.reset();
  });
  [
    "#account-form",
    "#users-panel",
    "#sip-panel",
    "#admin-alert-panel",
    "#install-voice",
    "#cancel-voice-install",
  ].forEach((s) => $(s).classList.add("hidden"));
  $("#settings-form").closest("section").querySelector("h2").textContent =
    "Voice and call alerts";
  if (state.previewVoiceAudio) {
    state.previewVoiceAudio.pause();
    state.previewVoiceAudio = null;
  }
  if (state.previewVoiceURL) {
    URL.revokeObjectURL(state.previewVoiceURL);
    state.previewVoiceURL = "";
  }
  state.previewVoiceBusy = false;
  state.voiceInstallBusy = false;
  state.activeVoiceJobID = "";
  $("#install-voice").disabled = false;
  $("#preview-voice").disabled = false;
  $("#cancel-voice-install").disabled = false;
  $("#cancel-voice-install").textContent = "Cancel installation";
  $("#voice-operation-controls").setAttribute("aria-busy", "false");
  $("#global-alerts-available").checked = false;
  $("#sip-failure").classList.add("hidden");
  $("#refresh-status").disabled = false;
  $("#operational-panel").setAttribute("aria-busy", "false");
  for (const name of [
    "readiness",
    "registration",
    "process",
    "bridge",
    "media",
  ])
    $("#runtime-" + name).textContent = "Checking…";
  $("#voice-progress").classList.add("hidden");
  $("#voice-progress").value = 0;
  ["#password-form", "#pin-form"].forEach((id) => {
    const f = $(id);
    f.dataset.pending = "false";
    f.setAttribute("aria-busy", "false");
    f.querySelectorAll("button").forEach((b) => (b.disabled = false));
  });
  document
    .querySelectorAll(".alert-control,#alert-controls")
    .forEach((el) => el.classList.remove("hidden"));
  $("#alert-controls-extra").classList.remove("hidden");
  $("#auth-status").textContent = "";
  $("#auth-status").className = "status";
  clearSensitiveFields();
  setAuthMode(
    document.body.dataset.setupAvailable === "true" ? "setup" : "login",
    false,
  );
  state.accountActionBusy = false;
  $("#accounts").setAttribute("aria-busy", "false");
  $("#account-form").setAttribute("aria-busy", "false");
  document
    .querySelectorAll("#account-form button,#accounts button")
    .forEach((b) => (b.disabled = false));
}

export function showSignedOut(message) {
  beginSession();
  state.csrf = "";
  clearSessionView();
  $("#app").classList.add("hidden");
  $("#auth").classList.remove("hidden");
  if (message) {
    $("#auth-status").textContent = message;
    $("#auth-status").className = "status error";
  }
}

export const public401Paths = new Set([
  "/api/v1/setup",
  "/api/v1/login",
  "/api/v1/recovery/request",
  "/api/v1/recovery/confirm",
  "/api/v1/me",
]);

export async function api(path, opts = {}) {
  const epoch = state.sessionEpoch,
    request = {
      ...opts,
      headers: {
        "Content-Type": "application/json",
        "X-CSRF-Token": state.csrf,
        ...(opts.headers || {}),
      },
      signal: state.sessionAbort.signal,
    };
  const r = await fetch(path, request);
  if (epoch !== state.sessionEpoch) throw changedSession();
  const token = r.headers.get("X-CSRF-Token");
  const b = await r.json().catch(() => ({}));
  if (epoch !== state.sessionEpoch) throw changedSession();
  if (token) state.csrf = token;
  if (r.status === 401) {
    if (!public401Paths.has(path) || (path === "/api/v1/me" && window.userId)) {
      showSignedOut(
        path === "/api/v1/me"
          ? ""
          : "Your session expired; please sign in again.",
      );
      throw changedSession();
    }
    throw Error(b.error || "authentication required");
  }
  if (!r.ok)
    throw Object.assign(Error(b.error || r.statusText), {
      status: r.status,
      code: b.code,
      requestId: b.request_id,
      fields: b.fields,
    });
  return b;
}

export function sessionDelay(ms, signal = state.sessionAbort.signal) {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(changedSession());
      return;
    }
    let timer;
    const done = () => {
      signal.removeEventListener("abort", cancel);
      resolve();
    };
    const cancel = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", cancel);
      reject(changedSession());
    };
    timer = setTimeout(done, ms);
    signal.addEventListener("abort", cancel, { once: true });
  });
}

export const formData = (f) => Object.fromEntries(new FormData(f));
