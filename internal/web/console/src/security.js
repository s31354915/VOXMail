// security: task behavior and its event bindings.
import { state } from "./state.js";
import { $, api } from "./core.js";

export function securityFieldError(form, name, message) {
  const input = form.elements.namedItem(name);
  if (!input || !input.setAttribute) return;
  input.setAttribute("aria-invalid", "true");
  $("#" + form.id + "-" + name + "-error").textContent = message;
}

export function clearSecurityErrors(form) {
  const forms = form
    ? [form]
    : [$("#username-form"), $("#password-form"), $("#pin-form"), $("#twofa-form")];
  forms.forEach((f) => {
    f.querySelectorAll("[aria-invalid]").forEach((el) =>
      el.removeAttribute("aria-invalid"),
    );
    f.querySelectorAll(".field-error").forEach((el) => (el.textContent = ""));
  });
}

export function securityError(form, error) {
  const el = form.querySelector(".status");
  el.textContent = error.message;
  el.classList.add("error");
  for (const [name, message] of Object.entries(error.fields || {}))
    securityFieldError(form, name, message);
  const first = form.querySelector("[aria-invalid=true]:not([disabled])");
  (first && first.offsetParent ? first : el).focus();
}

export function validateSecurity(form) {
  clearSecurityErrors(form);
  form.querySelector(".status").textContent = "";
  for (const input of form.querySelectorAll("input:not([disabled])")) {
    if (input.required && !input.value)
      securityFieldError(form, input.name, "This field is required.");
    else if (input.name === "new_password") {
      const bytes = new TextEncoder().encode(input.value).length;
      if (bytes < 12 || bytes > 72)
        securityFieldError(
          form,
          input.name,
          "Use a password of 12–72 UTF-8 bytes.",
        );
    } else if (input.name === "new_username") {
      const value = input.value.trim();
      if (value.length < 3 || value.length > 64)
        securityFieldError(form, input.name, "Use 3-64 characters.");
    } else if (input.name === "new_pin" && !/^[0-9]{4,12}$/.test(input.value))
      securityFieldError(form, input.name, "Use a PIN of 4–12 digits.");
    else if (input.name === "code" && !/^[0-9]{6}$/.test(input.value))
      securityFieldError(
        form,
        input.name,
        "Enter the six-digit code from your app.",
      );
    else if (
      input.name === "confirm_password" &&
      input.value !== form.elements.new_password.value
    )
      securityFieldError(form, input.name, "The passwords do not match.");
    else if (
      input.name === "confirm_pin" &&
      input.value !== form.elements.new_pin.value
    )
      securityFieldError(form, input.name, "The PINs do not match.");
  }
  const first = form.querySelector("[aria-invalid=true]");
  if (first) {
    form.querySelector(".status").textContent = "Check the highlighted fields.";
    first.focus();
    return false;
  }
  return true;
}

export async function saveSecurity(form, path, payload, message) {
  if (form.dataset.pending === "true" || !validateSecurity(form)) return;
  const epoch = state.sessionEpoch;
  form.dataset.pending = "true";
  form.setAttribute("aria-busy", "true");
  form.querySelectorAll("button").forEach((b) => (b.disabled = true));
  const output = form.querySelector(".status");
  output.classList.remove("error");
  output.textContent = "Saving…";
  try {
    await api(path, { method: "POST", body: JSON.stringify(payload) });
    if (epoch !== state.sessionEpoch) return;
    form.reset();
    clearSecurityErrors(form);
    output.textContent = message;
    output.focus();
  } catch (e) {
    if (e.name !== "AbortError" && epoch === state.sessionEpoch)
      securityError(form, e);
  } finally {
    if (epoch === state.sessionEpoch) {
      form.dataset.pending = "false";
      form.setAttribute("aria-busy", "false");
      form.querySelectorAll("button").forEach((b) => (b.disabled = false));
    }
  }
}

export function twoFactorMessage(message, error = false) {
  const el = $("#twofa-status");
  el.textContent = message;
  el.classList.toggle("error", error);
  if (error) el.focus();
}

export function renderTwoFactor() {
  const step = state.twoFactorStep,
    active = step !== "idle";
  $("#twofa-state").textContent = state.twoFactorEnabled
    ? "Authenticator protection is enabled."
    : "Authenticator protection is not enabled.";
  $("#twofa-steps").classList.toggle(
    "hidden",
    !["scan", "verify", "backup"].includes(step),
  );
  document.querySelectorAll("#twofa-steps li").forEach((el, i) => {
    if (i === { scan: 0, verify: 1, backup: 2 }[step])
      el.setAttribute("aria-current", "step");
    else el.removeAttribute("aria-current");
  });
  $("#twofa-password").classList.toggle("hidden", step === "backup");
  $("#twofa-form").elements.current_password.required = step !== "backup";
  $("#twofa-provision").classList.toggle("hidden", step !== "scan");
  $("#twofa-backup").classList.toggle("hidden", step !== "backup");
  const needsCode = step === "verify" || step === "disable";
  $("#twofa-code").classList.toggle("hidden", !needsCode);
  const code = $("#twofa-form").elements.code;
  code.disabled = !needsCode;
  code.required = needsCode;
  const visible = {
    "twofa-setup": !active && !state.twoFactorEnabled,
    "twofa-disable": !active && state.twoFactorEnabled,
    "twofa-next": step === "scan",
    "twofa-enable": step === "verify",
    "twofa-confirm-disable": step === "disable",
    "twofa-cancel": active && step !== "backup",
    "twofa-done": step === "backup",
  };
  Object.entries(visible).forEach(([id, show]) =>
    $("#" + id).classList.toggle("hidden", !show),
  );
  $("#twofa-cancel").textContent =
    step === "disable" ? "Cancel" : "Cancel setup";
  $("#twofa-form").setAttribute("aria-busy", String(state.twoFactorBusy));
  document
    .querySelectorAll("#twofa-form button")
    .forEach(
      (el) =>
        (el.disabled =
          state.twoFactorBusy ||
          (el.id === "twofa-done" && !$("#twofa-saved").checked)),
    );
}

export function clearTwoFactor(enabled = false) {
  state.twoFactorEpoch++;
  state.twoFactorEnabled = enabled;
  state.twoFactorStep = "idle";
  state.twoFactorBusy = false;
  $("#twofa-form").reset();
  clearSecurityErrors($("#twofa-form"));
  $("#twofa-qr").removeAttribute("src");
  $("#twofa-backup-codes").textContent = "";
  $("#twofa-status").textContent = "";
  renderTwoFactor();
}

export async function loadTwoFactor() {
  const u = await api("/api/v1/me");
  clearTwoFactor(!!u.two_factor_enabled);
}

export async function twoFactorAction(run) {
  if (state.twoFactorBusy) return;
  state.twoFactorBusy = true;
  const epoch = state.twoFactorEpoch;
  clearSecurityErrors($("#twofa-form"));
  twoFactorMessage("Working…");
  renderTwoFactor();
  let nextFocus;
  try {
    nextFocus = await run(epoch);
  } catch (e) {
    if (e.name !== "AbortError" && epoch === state.twoFactorEpoch)
      securityError($("#twofa-form"), e);
  } finally {
    if (epoch === state.twoFactorEpoch) {
      state.twoFactorBusy = false;
      renderTwoFactor();
      if (nextFocus) $(nextFocus).focus();
    }
  }
}

export function initializeSecurity() {
  document
    .querySelectorAll(
      "#username-form input,#password-form input,#pin-form input,#twofa-form input[name]",
    )
    .forEach((input) => {
      const form = input.form,
        id = form.id + "-" + input.name + "-error",
        error = document.createElement("span");
      error.id = id;
      error.className = "field-error";
      input.closest("label").appendChild(error);
      input.setAttribute(
        "aria-describedby",
        [input.getAttribute("aria-describedby"), id].filter(Boolean).join(" "),
      );
      input.oninput = () => {
        input.removeAttribute("aria-invalid");
        error.textContent = "";
      };
    });
  $("#password-form").onsubmit = (e) => {
    e.preventDefault();
    const f = e.target;
    saveSecurity(
      f,
      "/api/v1/security/password",
      {
        current_password: f.elements.current_password.value,
        new_password: f.elements.new_password.value,
      },
      "Password changed; other web sessions were signed out.",
    );
  };
  $("#username-form").onsubmit = (e) => {
    e.preventDefault();
    const f = e.target;
    saveSecurity(
      f,
      "/api/v1/security/username",
      {
        current_password: f.elements.current_password.value,
        new_username: f.elements.new_username.value,
      },
      "Username changed; other web sessions were signed out.",
    );
  };
  $("#pin-form").onsubmit = (e) => {
    e.preventDefault();
    const f = e.target;
    saveSecurity(
      f,
      "/api/v1/security/pin",
      {
        current_password: f.elements.current_password.value,
        new_pin: f.elements.new_pin.value,
      },
      "Telephone PIN changed; telephone sessions were signed out.",
    );
  };
  for (const name of ["username", "password", "pin"])
    $("#" + name + "-cancel").onclick = () => {
      const f = $("#" + name + "-form");
      f.reset();
      clearSecurityErrors(f);
      f.querySelector(".status").textContent = "";
      f.elements.current_password.focus();
    };
  $("#twofa-setup").onclick = () => {
    const f = $("#twofa-form");
    if (!validateSecurity(f)) return;
    twoFactorAction(async (epoch) => {
      const v = await api("/api/v1/security/2fa/setup", {
        method: "POST",
        body: JSON.stringify({
          current_password: f.elements.current_password.value,
        }),
      });
      if (epoch !== state.twoFactorEpoch) return;
      f.elements.secret.value = v.secret;
      $("#twofa-qr").src = v.qr_data_uri;
      state.twoFactorStep = "scan";
      twoFactorMessage("Scan the QR code, then continue to verify your app.");
      renderTwoFactor();
      return "#twofa-next";
    });
  };
  $("#twofa-next").onclick = () => {
    state.twoFactorStep = "verify";
    twoFactorMessage(
      "Enter the code from your authenticator app to enable protection.",
    );
    renderTwoFactor();
    $("#twofa-form").elements.code.focus();
  };
  $("#twofa-disable").onclick = () => {
    state.twoFactorStep = "disable";
    twoFactorMessage(
      "Confirm your password and authenticator code to disable protection.",
    );
    renderTwoFactor();
    $("#twofa-form").elements.current_password.focus();
  };
  $("#twofa-cancel").onclick = () => {
    clearTwoFactor(state.twoFactorEnabled);
    $("#" + (state.twoFactorEnabled ? "twofa-disable" : "twofa-setup")).focus();
  };
  $("#twofa-form").onsubmit = (e) => {
    e.preventDefault();
    const step = state.twoFactorStep;
    if (!["verify", "disable"].includes(step)) return;
    const f = e.target;
    if (!validateSecurity(f)) return;
    twoFactorAction(async (epoch) => {
      const v = await api(
        "/api/v1/security/2fa/" + (step === "verify" ? "enable" : "disable"),
        {
          method: "POST",
          body: JSON.stringify({
            current_password: f.elements.current_password.value,
            secret: f.elements.secret.value,
            code: f.elements.code.value,
          }),
        },
      );
      if (epoch !== state.twoFactorEpoch) return;
      f.reset();
      $("#twofa-qr").removeAttribute("src");
      if (step === "verify") {
        state.twoFactorEnabled = true;
        state.twoFactorStep = "backup";
        $("#twofa-backup-codes").textContent = v.backup_codes.join("\n");
        twoFactorMessage(
          "Enabled. Save your backup codes before finishing setup.",
        );
        renderTwoFactor();
        $("#twofa-backup-codes").focus();
      } else {
        clearTwoFactor();
        twoFactorMessage("Disabled. You can set up an authenticator again.");
        $("#twofa-setup").focus();
      }
    });
  };
  $("#twofa-download").onclick = () => {
    const text = $("#twofa-backup-codes").textContent;
    if (!text) return;
    const blob = new Blob(
        [
          "VOXMail backup codes — keep private. Each code can be used once.\n\n" +
            text +
            "\n",
        ],
        { type: "text/plain;charset=utf-8" },
      ),
      url = URL.createObjectURL(blob),
      a = document.createElement("a");
    a.href = url;
    a.download = "voxmail-backup-codes.txt";
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    twoFactorMessage(
      "Backup codes downloaded. Confirm that you saved them in a safe place.",
    );
  };
  $("#twofa-saved").onchange = renderTwoFactor;
  $("#twofa-done").onclick = () => {
    if (!$("#twofa-saved").checked) return;
    clearTwoFactor(true);
    twoFactorMessage("Enabled. Backup codes saved.");
    $("#twofa-disable").focus();
  };
}
