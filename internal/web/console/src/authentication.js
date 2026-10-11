// authentication: task behavior and its event bindings.
import { state } from "./state.js";
import {
  $,
  clearSensitiveFields,
  beginSession,
  api,
  formData,
  beginMutation,
  showSignedOut,
} from "./core.js";
import { loadAll } from "./bootstrap.js";

export function renderAuthState(setupAvailable) {
  const setup = !!setupAvailable;
  document.body.dataset.setupAvailable = String(setup);
  $("#auth-loading").classList.add("hidden");
  setAuthMode(setup ? "setup" : "login", false);
  $("#auth-eyebrow").textContent = setup
    ? "First-run setup"
    : "Private by design";
  $("#auth-heading").innerHTML = setup
    ? "Start with one secure administrator."
    : "Your mailbox, <br>ready when you are.";
  $("#auth-description").textContent = setup
    ? "Create the first administrator to unlock the console. You can add mail accounts and users after setup."
    : "VOXMail connects your trusted mail accounts to a simple telephone experience and a focused management console.";
}

export const initialSetupState = document.body.dataset.setupAvailable;

export function authMessage(message, error = false) {
  $("#auth-status").textContent = message;
  $("#auth-status").className = "status" + (error ? " error" : "");
}

export function authPending(pending) {
  state.authBusy = pending;
  document.querySelectorAll("#auth form").forEach((f) => {
    f.setAttribute("aria-busy", String(pending));
    f.querySelectorAll("button[type=submit]").forEach(
      (b) => (b.disabled = pending),
    );
  });
}

export function setAuthMode(mode, focus = true) {
  state.authEpoch++;
  beginSession();
  state.authMode = mode;
  authPending(false);
  authMessage("");
  ["#setup", "#login", "#recovery-request", "#recovery-confirm"].forEach(
    (id) => {
      $(id).reset();
      $(id).classList.toggle("hidden", id !== "#" + mode);
    },
  );
  $("#recovery-tools").classList.toggle("hidden", mode !== "login");
  $("#recovery-back").classList.toggle("hidden", !mode.startsWith("recovery-"));
  $("#auth").dataset.state = mode;
  clearSensitiveFields();
  if (focus) {
    const field = $("#" + mode).querySelector(
      "input:not([type=hidden]):not([readonly]):not([disabled])",
    );
    if (field) field.focus();
  }
}

export function openRecovery(purpose) {
  state.recoveryPurpose = purpose;
  setAuthMode("recovery-request");
  $("#recovery-request").elements.purpose.value = purpose;
  $("#recovery-title").textContent =
    purpose === "reset_password" ? "Reset password" : "Bypass TOTP";
  $("#recovery-request-copy").textContent =
    purpose === "reset_password"
      ? "Request a code through your configured mailbox to reset your password."
      : "Request a mailbox code to sign in once without your authenticator. Your 2FA stays enabled.";
}

export function confirmRecovery(email) {
  setAuthMode("recovery-confirm");
  const f = $("#recovery-confirm");
  f.elements.email.value = email;
  f.elements.purpose.value = state.recoveryPurpose;
  const reset = state.recoveryPurpose === "reset_password";
  $("#recovery-confirm-title").textContent = reset
    ? "Choose a new password"
    : "Sign in without TOTP";
  $("#recovery-password").classList.toggle("hidden", !reset);
  f.elements.new_password.disabled = !reset;
  f.elements.new_password.required = reset;
  f.elements.code.focus();
  authMessage("If the address is configured, a recovery code will be sent.");
}

export async function signIn(path, id) {
  if (state.authBusy) return;
  const epoch = state.authEpoch,
    payload = formData($(id));
  if (path === "/api/v1/setup") payload.sign_in = false;
  authPending(true);
  try {
    const u = await api(path, {
      method: "POST",
      body: JSON.stringify(payload),
    });
    if (epoch !== state.authEpoch) return;
    if (path === "/api/v1/setup") {
      renderAuthState(false);
      $("#login").elements.username.value = u.username;
      authMessage("Administrator created. Sign in to continue.");
      $("#login").elements.password.focus();
      return;
    }
    renderAuthState(false);
    beginSession();
    window.userId = u.id;
    window.isAdmin = u.role === "admin";
    $("#auth").classList.add("hidden");
    $("#app").classList.remove("hidden");
    $("#who").textContent = u.username;
    await loadAll();
  } catch (e) {
    if (e.name === "AbortError" || epoch !== state.authEpoch) return;
    if (path === "/api/v1/setup" && e.status === 409) {
      renderAuthState(false);
      authMessage("Setup was completed in another tab. Sign in to continue.");
      $("#login").elements.username.focus();
    } else {
      authMessage(e.message, true);
    }
  } finally {
    if (epoch === state.authEpoch) authPending(false);
  }
}

export function initializeAuthentication() {
  if (initialSetupState === "true" || initialSetupState === "false")
    renderAuthState(initialSetupState === "true");
  $("#reset-password").onclick = () => openRecovery("reset_password");
  $("#bypass-totp").onclick = () => openRecovery("bypass_2fa");
  $("#recovery-back").onclick = () => setAuthMode("login");
  $("#setup").onsubmit = (e) => {
    e.preventDefault();
    signIn("/api/v1/setup", "#setup");
  };
  $("#login").onsubmit = (e) => {
    e.preventDefault();
    signIn("/api/v1/login", "#login");
  };
  $("#recovery-request").onsubmit = async (e) => {
    e.preventDefault();
    if (state.authBusy) return;
    const epoch = state.authEpoch,
      payload = formData(e.target);
    authPending(true);
    authMessage("Sending recovery request…");
    try {
      await api("/api/v1/recovery/request", {
        method: "POST",
        body: JSON.stringify(payload),
      });
      if (epoch !== state.authEpoch) return;
      confirmRecovery(payload.email);
    } catch (x) {
      if (x.name !== "AbortError" && epoch === state.authEpoch)
        authMessage(x.message, true);
    } finally {
      if (epoch === state.authEpoch) authPending(false);
    }
  };
  $("#recovery-confirm").onsubmit = async (e) => {
    e.preventDefault();
    if (state.authBusy) return;
    const epoch = state.authEpoch;
    authPending(true);
    authMessage("Checking recovery code…");
    try {
      const v = await api("/api/v1/recovery/confirm", {
        method: "POST",
        body: JSON.stringify(formData(e.target)),
      });
      if (epoch !== state.authEpoch) return;
      if (v.status === "signed_in") {
        renderAuthState(false);
        beginSession();
        window.userId = v.user.id;
        window.isAdmin = v.user.role === "admin";
        $("#auth").classList.add("hidden");
        $("#app").classList.remove("hidden");
        $("#who").textContent = v.user.username;
        await loadAll();
      } else {
        setAuthMode("login");
        authMessage("Password reset. Sign in with your new password.");
      }
    } catch (x) {
      if (x.name !== "AbortError" && epoch === state.authEpoch)
        authMessage(x.message, true);
    } finally {
      if (epoch === state.authEpoch) authPending(false);
    }
  };
  $("#logout").onclick = async () => {
    const button = $("#logout");
    if (!beginMutation(button)) return;
    const epoch = state.sessionEpoch;
    try {
      await api("/api/v1/logout", { method: "POST" });
    } catch (_) {}
    if (epoch === state.sessionEpoch) {
      showSignedOut("");
      location.reload();
    }
  };
}
