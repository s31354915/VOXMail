(() => {
  // internal/web/console/src/state.js
  var state = {
    operationalTimer: null,
    operationalBusy: false,
    operationalRequest: null,
    syncSeenStates: /* @__PURE__ */ new Map(),
    csrf: "",
    accounts: [],
    contacts: [],
    alertsAvailable: true,
    sessionEpoch: 0,
    sessionAbort: new AbortController(),
    authMode: "login",
    recoveryPurpose: "reset_password",
    authBusy: false,
    authEpoch: 0,
    voiceInstallBusy: false,
    activeVoiceJobID: "",
    previewVoiceBusy: false,
    previewVoiceAudio: null,
    previewVoiceURL: "",
    accountActionBusy: false,
    twoFactorEnabled: false,
    twoFactorStep: "idle",
    twoFactorBusy: false,
    twoFactorEpoch: 0
  };

  // internal/web/console/src/security.js
  function securityFieldError(form, name, message2) {
    const input = form.elements.namedItem(name);
    if (!input || !input.setAttribute) return;
    input.setAttribute("aria-invalid", "true");
    $("#" + form.id + "-" + name + "-error").textContent = message2;
  }
  function clearSecurityErrors(form) {
    const forms = form ? [form] : [$("#username-form"), $("#password-form"), $("#pin-form"), $("#twofa-form")];
    forms.forEach((f) => {
      f.querySelectorAll("[aria-invalid]").forEach(
        (el) => el.removeAttribute("aria-invalid")
      );
      f.querySelectorAll(".field-error").forEach((el) => el.textContent = "");
    });
  }
  function securityError(form, error) {
    const el = form.querySelector(".status");
    el.textContent = error.message;
    el.classList.add("error");
    for (const [name, message2] of Object.entries(error.fields || {}))
      securityFieldError(form, name, message2);
    const first = form.querySelector("[aria-invalid=true]:not([disabled])");
    (first && first.offsetParent ? first : el).focus();
  }
  function validateSecurity(form) {
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
            "Use a password of 12\u201372 UTF-8 bytes."
          );
      } else if (input.name === "new_username") {
        const value = input.value.trim();
        if (value.length < 3 || value.length > 64)
          securityFieldError(form, input.name, "Use 3-64 characters.");
      } else if (input.name === "new_pin" && !/^[0-9]{4,12}$/.test(input.value))
        securityFieldError(form, input.name, "Use a PIN of 4\u201312 digits.");
      else if (input.name === "code" && !/^[0-9]{6}$/.test(input.value))
        securityFieldError(
          form,
          input.name,
          "Enter the six-digit code from your app."
        );
      else if (input.name === "confirm_password" && input.value !== form.elements.new_password.value)
        securityFieldError(form, input.name, "The passwords do not match.");
      else if (input.name === "confirm_pin" && input.value !== form.elements.new_pin.value)
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
  async function saveSecurity(form, path, payload, message2) {
    if (form.dataset.pending === "true" || !validateSecurity(form)) return;
    const epoch = state.sessionEpoch;
    form.dataset.pending = "true";
    form.setAttribute("aria-busy", "true");
    form.querySelectorAll("button").forEach((b) => b.disabled = true);
    const output = form.querySelector(".status");
    output.classList.remove("error");
    output.textContent = "Saving\u2026";
    try {
      await api(path, { method: "POST", body: JSON.stringify(payload) });
      if (epoch !== state.sessionEpoch) return;
      form.reset();
      clearSecurityErrors(form);
      output.textContent = message2;
      output.focus();
    } catch (e) {
      if (e.name !== "AbortError" && epoch === state.sessionEpoch)
        securityError(form, e);
    } finally {
      if (epoch === state.sessionEpoch) {
        form.dataset.pending = "false";
        form.setAttribute("aria-busy", "false");
        form.querySelectorAll("button").forEach((b) => b.disabled = false);
      }
    }
  }
  function twoFactorMessage(message2, error = false) {
    const el = $("#twofa-status");
    el.textContent = message2;
    el.classList.toggle("error", error);
    if (error) el.focus();
  }
  function renderTwoFactor() {
    const step = state.twoFactorStep, active = step !== "idle";
    $("#twofa-state").textContent = state.twoFactorEnabled ? "Authenticator protection is enabled." : "Authenticator protection is not enabled.";
    $("#twofa-steps").classList.toggle(
      "hidden",
      !["scan", "verify", "backup"].includes(step)
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
      "twofa-done": step === "backup"
    };
    Object.entries(visible).forEach(
      ([id, show]) => $("#" + id).classList.toggle("hidden", !show)
    );
    $("#twofa-cancel").textContent = step === "disable" ? "Cancel" : "Cancel setup";
    $("#twofa-form").setAttribute("aria-busy", String(state.twoFactorBusy));
    document.querySelectorAll("#twofa-form button").forEach(
      (el) => el.disabled = state.twoFactorBusy || el.id === "twofa-done" && !$("#twofa-saved").checked
    );
  }
  function clearTwoFactor(enabled = false) {
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
  async function loadTwoFactor() {
    const u = await api("/api/v1/me");
    clearTwoFactor(!!u.two_factor_enabled);
  }
  async function twoFactorAction(run) {
    if (state.twoFactorBusy) return;
    state.twoFactorBusy = true;
    const epoch = state.twoFactorEpoch;
    clearSecurityErrors($("#twofa-form"));
    twoFactorMessage("Working\u2026");
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
  function initializeSecurity() {
    document.querySelectorAll(
      "#username-form input,#password-form input,#pin-form input,#twofa-form input[name]"
    ).forEach((input) => {
      const form = input.form, id = form.id + "-" + input.name + "-error", error = document.createElement("span");
      error.id = id;
      error.className = "field-error";
      input.closest("label").appendChild(error);
      input.setAttribute(
        "aria-describedby",
        [input.getAttribute("aria-describedby"), id].filter(Boolean).join(" ")
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
          new_password: f.elements.new_password.value
        },
        "Password changed; other web sessions were signed out."
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
          new_username: f.elements.new_username.value
        },
        "Username changed; other web sessions were signed out."
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
          new_pin: f.elements.new_pin.value
        },
        "Telephone PIN changed; telephone sessions were signed out."
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
            current_password: f.elements.current_password.value
          })
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
        "Enter the code from your authenticator app to enable protection."
      );
      renderTwoFactor();
      $("#twofa-form").elements.code.focus();
    };
    $("#twofa-disable").onclick = () => {
      state.twoFactorStep = "disable";
      twoFactorMessage(
        "Confirm your password and authenticator code to disable protection."
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
              code: f.elements.code.value
            })
          }
        );
        if (epoch !== state.twoFactorEpoch) return;
        f.reset();
        $("#twofa-qr").removeAttribute("src");
        if (step === "verify") {
          state.twoFactorEnabled = true;
          state.twoFactorStep = "backup";
          $("#twofa-backup-codes").textContent = v.backup_codes.join("\n");
          twoFactorMessage(
            "Enabled. Save your backup codes before finishing setup."
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
          "VOXMail backup codes \u2014 keep private. Each code can be used once.\n\n" + text + "\n"
        ],
        { type: "text/plain;charset=utf-8" }
      ), url = URL.createObjectURL(blob), a = document.createElement("a");
      a.href = url;
      a.download = "voxmail-backup-codes.txt";
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1e3);
      twoFactorMessage(
        "Backup codes downloaded. Confirm that you saved them in a safe place."
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

  // internal/web/console/src/core.js
  var $ = (s) => document.querySelector(s);
  var esc = (v) => String(v ?? "").replace(
    /[<>&"']/g,
    (c) => ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" })[c]
  );
  var status = (msg, error = false) => {
    if (!window.userId || !msg) return;
    const output = $("#app-status");
    output.textContent = msg;
    output.className = "status" + (error ? " error" : "");
    if (!msg.endsWith("\u2026")) output.focus();
  };
  function operationFeedback(target, message2, error = true) {
    if (!window.userId) return;
    const element = typeof target === "string" ? $(target) : target;
    if (!element) return status(message2, error);
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
    output.textContent = message2;
    output.classList.toggle("error", error);
    output.focus();
  }
  function clearOperationFeedback(target) {
    const element = typeof target === "string" ? $(target) : target;
    if (!element) return;
    const output = document.getElementById(
      element.getAttribute("id") + "-operation-feedback"
    );
    if (output) {
      output.textContent = "";
      output.classList.remove("error");
    }
  }
  function setMutationPending(target, pending, selector = "button") {
    const element = typeof target === "string" ? $(target) : target;
    if (!element) return;
    if (pending && element.dataset.mutationPending === "true") return false;
    element.dataset.mutationPending = String(pending);
    element.setAttribute("aria-busy", String(pending));
    if (element.matches("button")) element.disabled = pending;
    element.querySelectorAll(selector).forEach((button) => button.disabled = pending);
    return true;
  }
  function beginMutation(target, selector = "button") {
    return setMutationPending(target, true, selector);
  }
  function endMutation(target, selector = "button") {
    setMutationPending(target, false, selector);
  }
  function applyMutationPending(target, selector = "button") {
    const element = typeof target === "string" ? $(target) : target;
    if (element?.dataset.mutationPending === "true") {
      if (element.matches("button")) element.disabled = true;
      element.querySelectorAll(selector).forEach((button) => button.disabled = true);
    }
  }
  function clearSensitiveFields() {
    document.querySelectorAll(
      "input[type=password],input[name=pin],input[name=totp],input[name=backup_code],input[name=code],input[name=secret]"
    ).forEach((x) => x.value = "");
  }
  function changedSession() {
    const e = new Error("");
    e.name = "AbortError";
    return e;
  }
  function beginSession() {
    state.sessionEpoch++;
    state.sessionAbort.abort();
    state.sessionAbort = new AbortController();
    clearTimeout(state.operationalTimer);
    state.operationalTimer = null;
    state.operationalBusy = false;
    state.operationalRequest = null;
  }
  function clearSessionView() {
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
      "#sync-freshness"
    ].forEach((s) => {
      const e = $(s);
      if (e) e.textContent = "";
    });
    document.querySelectorAll('[data-mutation-pending="true"]').forEach((el) => {
      el.dataset.mutationPending = "false";
      el.setAttribute("aria-busy", "false");
      if (el.matches("button")) el.disabled = false;
      el.querySelectorAll("button").forEach(
        (button) => button.disabled = false
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
      "#twofa-form"
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
      "#cancel-voice-install"
    ].forEach((s) => $(s).classList.add("hidden"));
    $("#settings-form").closest("section").querySelector("h2").textContent = "Voice and call alerts";
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
      "media"
    ])
      $("#runtime-" + name).textContent = "Checking\u2026";
    $("#voice-progress").classList.add("hidden");
    $("#voice-progress").value = 0;
    ["#password-form", "#pin-form"].forEach((id) => {
      const f = $(id);
      f.dataset.pending = "false";
      f.setAttribute("aria-busy", "false");
      f.querySelectorAll("button").forEach((b) => b.disabled = false);
    });
    document.querySelectorAll(".alert-control,#alert-controls").forEach((el) => el.classList.remove("hidden"));
    $("#alert-controls-extra").classList.remove("hidden");
    $("#auth-status").textContent = "";
    $("#auth-status").className = "status";
    clearSensitiveFields();
    setAuthMode(
      document.body.dataset.setupAvailable === "true" ? "setup" : "login",
      false
    );
    state.accountActionBusy = false;
    $("#accounts").setAttribute("aria-busy", "false");
    $("#account-form").setAttribute("aria-busy", "false");
    document.querySelectorAll("#account-form button,#accounts button").forEach((b) => b.disabled = false);
  }
  function showSignedOut(message2) {
    beginSession();
    state.csrf = "";
    clearSessionView();
    $("#app").classList.add("hidden");
    $("#auth").classList.remove("hidden");
    if (message2) {
      $("#auth-status").textContent = message2;
      $("#auth-status").className = "status error";
    }
  }
  var public401Paths = /* @__PURE__ */ new Set([
    "/api/v1/setup",
    "/api/v1/login",
    "/api/v1/recovery/request",
    "/api/v1/recovery/confirm",
    "/api/v1/me"
  ]);
  async function api(path, opts = {}) {
    const epoch = state.sessionEpoch, request = {
      ...opts,
      headers: {
        "Content-Type": "application/json",
        "X-CSRF-Token": state.csrf,
        ...opts.headers || {}
      },
      signal: state.sessionAbort.signal
    };
    const r = await fetch(path, request);
    if (epoch !== state.sessionEpoch) throw changedSession();
    const token = r.headers.get("X-CSRF-Token");
    const b = await r.json().catch(() => ({}));
    if (epoch !== state.sessionEpoch) throw changedSession();
    if (token) state.csrf = token;
    if (r.status === 401) {
      if (!public401Paths.has(path) || path === "/api/v1/me" && window.userId) {
        showSignedOut(
          path === "/api/v1/me" ? "" : "Your session expired; please sign in again."
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
        fields: b.fields
      });
    return b;
  }
  function sessionDelay(ms, signal = state.sessionAbort.signal) {
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
  var formData = (f) => Object.fromEntries(new FormData(f));

  // internal/web/console/src/operations.js
  function timestamp(value) {
    const date = new Date(value);
    return value && Number.isFinite(date.getTime()) ? '<time datetime="' + esc(value) + '" title="' + esc(date.toISOString()) + '">' + esc(date.toLocaleString()) + "</time>" : "Unknown time";
  }
  function failureDetails(f) {
    return f ? '<div class="operational-failure"><p><img class="failure-icon" src="/assets/console-mailboxes-failure-icon.webp" alt="" width="18" height="18"> ' + esc(f.reason) + "</p><p>" + esc(f.action) + "</p>" + (f.diagnostic ? '<p class="muted">' + esc(f.diagnostic) + "</p>" : "") + "</div>" : "";
  }
  function syncDetails(a) {
    const run = a.last_sync;
    if (!run) return '<span class="sync-state">Never synchronized</span>';
    const state2 = run.state || (run.finished_at ? run.success ? "succeeded" : "failed" : "unfinished"), names2 = {
      running: "Synchronizing\u2026",
      unfinished: "Unfinished sync \u2014 worker not confirmed",
      succeeded: "Synchronized",
      failed: "Sync failed"
    }, last = a.last_successful_sync || (run.success ? run : null), progress = state2 === "running" ? '<progress aria-label="Mailbox synchronization in progress"></progress>' : "";
    const cancel = state2 === "running" && run.kind === "manual" ? '<button type="button" class="link" data-cancel-sync="' + esc(a.id) + '">Cancel sync</button>' : "";
    return '<span class="sync-state">' + esc(names2[state2] || "Unknown sync state") + "</span>" + progress + cancel + (state2 === "running" ? "<p>Mail is still synchronizing. You can cancel this manual run and retry later.</p>" : "") + "<p>Started: " + timestamp(run.started_at) + "</p>" + (run.finished_at ? "<p>Finished: " + timestamp(run.finished_at) + "</p>" : "") + "<p>Last successful sync: " + (last ? timestamp(last.finished_at) : "Never") + "</p>" + failureDetails(run.failure) + '<p class="muted">Run ' + esc(run.id) + " \xB7 " + esc(run.kind) + "</p>";
  }
  function renderRuntime(snapshot) {
    const names2 = {
      ready: "Ready",
      not_ready: "Not ready",
      registered: "Registered",
      registering: "Registering\u2026",
      failed: "Failed",
      unregistered: "Unregistered",
      unknown: "Unknown",
      disabled: "Disabled",
      starting: "Starting\u2026",
      running: "Running",
      stopping: "Stopping\u2026",
      stopped: "Stopped",
      degraded: "Degraded",
      connected: "Connected",
      disconnected: "Disconnected",
      unavailable: "Unavailable",
      not_required: "Not required"
    };
    const runtime = snapshot.runtime || {};
    for (const key of ["process", "bridge", "registration", "media"])
      $("#runtime-" + key).textContent = names2[runtime[key]] || "Unknown";
    $("#runtime-readiness").textContent = snapshot.offline_mode ? "Offline / maintenance" : names2[snapshot.readiness] || "Unknown";
    $("#operational-updated").innerHTML = "Last checked: " + timestamp(snapshot.checked_at) + (snapshot.offline_mode ? " \xB7 Provider operations are disabled." : "");
    const sipFailure = $("#sip-failure"), failureHTML = failureDetails(snapshot.sip_failure);
    if (sipFailure.innerHTML !== failureHTML) sipFailure.innerHTML = failureHTML;
    sipFailure.classList.toggle("hidden", !snapshot.sip_failure);
    const cleanup = snapshot.cleanup || {};
    const cleanupMessages = [];
    if (cleanup.pending > 0)
      cleanupMessages.push(
        cleanup.pending + " stored-file cleanup job(s) pending."
      );
    if (cleanup.failed > 0)
      cleanupMessages.push(
        cleanup.failed + " stored-file cleanup job(s) failed. Check storage permissions and server logs; cleanup retries automatically after five minutes."
      );
    $("#operational-status").textContent = [
      snapshot.reason || "",
      ...cleanupMessages
    ].filter(Boolean).join(" ");
    $("#operational-status").classList.toggle(
      "error",
      snapshot.readiness === "not_ready" || cleanup.failed > 0
    );
  }
  function refreshOperationalStatus(announce = false) {
    if (!window.userId) return Promise.resolve();
    if (state.operationalBusy) {
      const queuedEpoch = state.sessionEpoch, current = state.operationalRequest;
      if (!announce || !current) return current || Promise.resolve();
      const retry = () => queuedEpoch === state.sessionEpoch && window.userId ? refreshOperationalStatus(true) : void 0;
      return current.then(retry, retry);
    }
    const epoch = state.sessionEpoch;
    state.operationalBusy = true;
    $("#refresh-status").disabled = true;
    $("#operational-panel").setAttribute("aria-busy", "true");
    const request = (async () => {
      try {
        const results = await Promise.allSettled([
          api("/api/v1/status"),
          api("/api/v1/accounts")
        ]);
        if (epoch !== state.sessionEpoch) return;
        const [service, mail] = results;
        const serviceOK = service.status === "fulfilled" && service.value && service.value.runtime;
        const mailOK = mail.status === "fulfilled" && Array.isArray(mail.value);
        if (serviceOK) {
          renderRuntime(service.value);
        } else {
          const output = $("#operational-status");
          output.textContent = "Service status could not be refreshed. The last displayed snapshot may be stale. Use Refresh status to retry.";
          output.classList.add("error");
        }
        if (mailOK) {
          const latest = new Map(mail.value.map((a) => [a.id, a]));
          for (const a of state.accounts) {
            const current = latest.get(a.id);
            if (current) {
              a.last_sync = current.last_sync;
              a.last_successful_sync = current.last_successful_sync;
            }
          }
          document.querySelectorAll("[data-sync-status]").forEach((el) => {
            const a = latest.get(el.dataset.syncStatus);
            if (a) {
              const details = syncDetails(a);
              if (el.innerHTML !== details) el.innerHTML = details;
            } else if (el.textContent !== "This account is no longer available. Reload the account list.")
              el.textContent = "This account is no longer available. Reload the account list.";
          });
          updateSyncButtons();
          const changes = [];
          for (const account of mail.value) {
            const run = account.last_sync, key = run ? `${run.id}:${run.state}:${run.failure?.category || ""}:${run.finished_at || ""}` : "never";
            const previous = state.syncSeenStates.get(account.id);
            if (previous !== void 0 && previous !== key)
              changes.push(
                `${account.canonical_name}: ${run?.state === "failed" ? "synchronization failed" : run?.state === "succeeded" ? "synchronization completed" : run?.state === "running" ? "synchronization started" : "synchronization state changed"}.`
              );
            state.syncSeenStates.set(account.id, key);
          }
          if (changes.length)
            $("#sync-change-announcement").textContent = changes.join(" ");
          $("#sync-freshness").textContent = "Sync status checked " + (/* @__PURE__ */ new Date()).toLocaleTimeString() + ". Updates automatically every 10 seconds.";
          $("#sync-freshness").classList.remove("sync-stale");
        } else {
          $("#sync-freshness").textContent = "Sync status could not be refreshed. Displayed sync information may be stale. Use Refresh status to retry.";
          $("#sync-freshness").classList.add("sync-stale");
        }
        if (announce) {
          const output = $("#refresh-status-announcement");
          output.textContent = serviceOK && mailOK ? "Service status and mailbox sync status refreshed." : serviceOK ? "Service status refreshed; mailbox sync status could not be refreshed." : mailOK ? "Mailbox sync status refreshed; service status could not be refreshed." : "Status could not be refreshed. Previous details may be stale.";
          output.focus();
        }
      } finally {
        if (epoch === state.sessionEpoch) {
          state.operationalBusy = false;
          $("#refresh-status").disabled = false;
          $("#operational-panel").setAttribute("aria-busy", "false");
        }
        if (state.operationalRequest === request) state.operationalRequest = null;
      }
    })();
    state.operationalRequest = request;
    return request;
  }
  function scheduleOperationalPoll() {
    clearTimeout(state.operationalTimer);
    const epoch = state.sessionEpoch;
    state.operationalTimer = setTimeout(async () => {
      if (epoch !== state.sessionEpoch || !window.userId) return;
      if (!document.hidden) await refreshOperationalStatus();
      if (epoch === state.sessionEpoch && window.userId)
        scheduleOperationalPoll();
    }, 1e4);
  }
  function initializeOperations() {
    $("#refresh-status").onclick = async () => {
      const epoch = state.sessionEpoch;
      await refreshOperationalStatus(true);
      if (epoch === state.sessionEpoch && window.userId)
        scheduleOperationalPoll();
    };
  }
  function updateSyncButtons() {
    document.querySelectorAll("[data-sync-account]").forEach((button) => {
      const account = state.accounts.find(
        (a) => a.id === button.dataset.syncAccount
      );
      button.disabled = state.accountActionBusy || account?.last_sync?.state === "running";
    });
  }

  // internal/web/console/src/folder-preview.js
  var fixed = ["inbox", "drafts", "sent", "trash"];
  var remoteKey = (name) => name.toUpperCase() === "INBOX" ? "INBOX" : name;
  var normalize = (name) => name.startsWith("/groups/") ? name : name.trim().replaceAll("\\", "/").replace(/^\/+|\/+$/g, "");
  var compare = (a, b) => {
    const left = Array.from(a), right = Array.from(b);
    for (let i = 0; i < Math.min(left.length, right.length); i++) {
      const difference = left[i].codePointAt(0) - right[i].codePointAt(0);
      if (difference) return difference;
    }
    return left.length - right.length;
  };
  function folderPreviewModel({
    folders,
    mapping,
    roles,
    policy,
    mailboxes = [],
    bindings = {}
  }) {
    const excluded2 = new Set((policy.excluded || []).map(remoteKey));
    const groups = policy.groups || [];
    const entries = [], byRemote = /* @__PURE__ */ new Map(), physical = /* @__PURE__ */ new Map();
    const localFor = (remote) => {
      const alias = Object.entries(mapping).find(
        ([name]) => remoteKey(name) === remoteKey(remote)
      )?.[1];
      return normalize(
        alias || (remoteKey(remote) === "INBOX" ? "Inbox" : remote)
      ).split("/").filter((part) => part && part !== ".").join("/");
    };
    const add = (entry) => {
      entries.push(entry);
      for (const remote of entry.folders) byRemote.set(remoteKey(remote), entry);
      for (const local of entry.members) physical.set(normalize(local), entry);
    };
    for (const group of groups) {
      add({
        key: `/groups/${group.id}`,
        name: group.name || "Unnamed group",
        folders: [...group.folders],
        primary: group.primary,
        members: group.folders.map(localFor)
      });
    }
    for (const remote of Object.keys(folders)) {
      if (excluded2.has(remoteKey(remote)) || byRemote.has(remoteKey(remote)))
        continue;
      const local = localFor(remote);
      add({
        key: local,
        name: local,
        folders: [remote],
        primary: remote,
        members: [local]
      });
    }
    for (const mailbox of mailboxes) {
      if (mailbox.key.startsWith("/groups/") || byRemote.has(remoteKey(mailbox.primary_remote)) || excluded2.has(remoteKey(mailbox.primary_remote)))
        continue;
      if (Object.keys(folders).some(
        (remote) => remoteKey(remote) === remoteKey(mailbox.primary_remote)
      ))
        continue;
      add({
        key: mailbox.key,
        name: mailbox.name,
        folders: [mailbox.primary_remote],
        primary: mailbox.primary_remote,
        members: mailbox.members
      });
    }
    for (const [local, remote] of Object.entries(bindings)) {
      const entry = byRemote.get(remoteKey(remote));
      if (entry) physical.set(normalize(local), entry);
    }
    const roleEntries = fixed.map((role) => {
      if (roles[role]) return byRemote.get(remoteKey(roles[role])) || null;
      const conventional = Object.keys(mapping).filter(
        (remote) => remote.trim().toLowerCase() === role
      );
      if (conventional.length > 1) return null;
      if (conventional.length === 1) {
        const candidate = byRemote.get(remoteKey(conventional[0]));
        if (candidate) return candidate;
        if (role === "inbox") return null;
      }
      if (role === "inbox") return physical.get("Inbox") || null;
      const matches = [...physical].filter(
        ([local]) => local.toLowerCase() === role
      );
      return matches.length === 1 ? matches[0][1] : null;
    });
    return { entries, roleEntries };
  }
  function folderPreviewPage(model, prefix = "", page = 0) {
    const labels = new Map(model.entries.map((entry) => [entry.key, entry]));
    const children = /* @__PURE__ */ new Set();
    prefix = normalize(prefix);
    for (const entry of model.entries) {
      const folder = normalize(entry.key);
      if (folder.startsWith("/groups/")) {
        if (!prefix) children.add(folder);
        continue;
      }
      if (!folder || folder === prefix || prefix && !folder.startsWith(prefix + "/"))
        continue;
      const rest = prefix ? folder.slice(prefix.length + 1) : folder;
      const child = rest.split("/")[0];
      children.add(prefix ? prefix + "/" + child : child);
    }
    const assigned = new Set(
      model.roleEntries.filter(Boolean).map((entry) => normalize(entry.key))
    );
    const others = [...children].filter((key) => prefix || !assigned.has(key)).map(
      (key) => labels.get(key) || { key, name: key, folders: [], members: [] }
    ).sort((a, b) => compare(a.name, b.name) || compare(a.key, b.key));
    const start = prefix ? page * 8 : page === 0 ? 0 : 4 + (page - 1) * 8;
    const limit = !prefix && page === 0 ? 4 : 8;
    const keyStart = !prefix && page === 0 ? 5 : 1;
    const rows = others.slice(start, start + limit).map((entry, index) => ({ key: keyStart + index, entry }));
    if (!prefix && page === 0)
      rows.unshift(
        ...model.roleEntries.map((entry, index) => ({
          key: index + 1,
          role: fixed[index],
          entry
        }))
      );
    return { rows, more: start + limit < others.length };
  }

  // internal/web/console/src/folder-editor.js
  var roleNames = {
    inbox: "Inbox",
    drafts: "Drafts",
    sent: "Sent",
    trash: "Trash",
    spam: "Spam",
    archive: "Archive"
  };
  var fixed2 = ["inbox", "drafts", "sent", "trash"];
  var editor = null;
  var opening = 0;
  document.addEventListener(
    "voxmail-session-cleared",
    () => closeFolderEditor(false)
  );
  function closeFolderEditor(focus = true) {
    opening++;
    const id = editor?.id;
    editor = null;
    $("#folder-editor").classList.add("hidden");
    $("#folder-editor").replaceChildren();
    if (focus && id)
      document.querySelector(`[data-folders-account="${CSS.escape(id)}"]`)?.focus();
  }
  async function openFolderEditor(account, afterSave) {
    var _a, _b;
    const epoch = state.sessionEpoch, request = ++opening;
    const data = await api(
      `/api/v1/accounts/${encodeURIComponent(account.id)}/folder-policy`
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
      mapping: Object.assign(/* @__PURE__ */ Object.create(null), data.folder_map),
      savedMapping: { ...data.folder_map },
      roles: { ...data.folder_roles },
      policy: structuredClone(data.policy || {}),
      selected: /* @__PURE__ */ new Set(),
      active: "",
      busy: false,
      tab: "folders",
      query: "",
      page: 0,
      pageSize: window.matchMedia("(max-width: 600px)").matches ? 5 : 10,
      afterSave
    };
    (_a = editor.policy).groups || (_a.groups = []);
    (_b = editor.policy).excluded || (_b.excluded = []);
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
    return Object.keys(editor.mapping).find(
      (name) => remoteKey(name) === remoteKey(remote)
    ) || remote;
  }
  function uniqueRemotes(remotes) {
    const seen = /* @__PURE__ */ new Set();
    return remotes.filter((remote) => {
      const key = remoteKey(remote);
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
  }
  function groupFor(remote) {
    return editor.policy.groups.find(
      (group) => group.folders.some((member) => remoteKey(member) === remoteKey(remote))
    );
  }
  function excluded(remote) {
    return editor.policy.excluded.some(
      (member) => remoteKey(member) === remoteKey(remote)
    );
  }
  function names() {
    return Object.keys(editor.folders).sort(
      (a, b) => a.localeCompare(b) || (a < b ? -1 : 1)
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
    const choices = /* @__PURE__ */ new Set([
      current,
      ...Object.values(roleNames),
      ...Object.values(editor.folders),
      ...Object.values(editor.mapping)
    ]);
    const localKey = (name) => name.replaceAll("\\", "/").split("/").filter((part) => part && part !== ".").join("/").toLowerCase();
    const occupied = [
      ...Object.entries(editor.folders),
      ...Object.entries(editor.savedMapping),
      ...Object.entries(editor.mapping),
      ...Object.entries(editor.bindings).map(([local, owner]) => [owner, local])
    ];
    const used = new Set(occupied.map(([, local]) => localKey(local)));
    let suggested = `Separate/${remote}`;
    for (let suffix = 2; used.has(localKey(suggested)); suffix++)
      suggested = `Separate/${remote}-${suffix}`;
    choices.add(suggested);
    return [...choices].filter(Boolean).sort((a, b) => a.localeCompare(b)).map((name) => {
      const taken = occupied.some(
        ([owner, local]) => remoteKey(owner) !== remoteKey(remote) && localKey(local) === localKey(name)
      );
      return `<option value="${esc(name)}"${name === current ? " selected" : ""}${taken && name !== current ? " disabled" : ""}>${esc(name)}${taken && name !== current ? " (in use \u2014 combine folders instead)" : ""}</option>`;
    }).join("");
  }
  function preview() {
    if (editor.warnings.length)
      return "<p>Resolve the folder mapping conflicts and save to refresh the phone preview.</p>";
    const model = folderPreviewModel(editor);
    const icons = [
      "added-083-envelope.svg",
      "cutout-91-3bccf74ae991.png",
      "added-091-paper-plane.svg",
      "added-095-trash-can.svg"
    ];
    const renderPage = (prefix = "", page = 0) => {
      const result = folderPreviewPage(model, prefix, page);
      let html = result.rows.map(({ key, entry, role }) => {
        const row = `<div class="mailbox" data-preview-folder="${esc(entry?.key || "")}" data-preview-key="${key}"><span class="number">${key}</span><img class="mailbox-icon" src="/assets/${role ? icons[fixed2.indexOf(role)] : "added-099-folder.svg"}" alt="" width="22" height="22"><span>${esc(roleNames[role] || entry.name)}<small>${entry ? esc(entry.name) + (entry.folders.length > 1 ? ` \xB7 ${entry.folders.length} folders \xB7 Primary: ${esc(entry.primary || "choose a folder")}` : "") : "Not configured"}</small></span></div>`;
        const nested = entry && folderPreviewPage(model, entry.key).rows.length > 0;
        return row + (nested ? `<details class="local-alias" data-preview-nested="${esc(entry.key)}"><summary>Nested folders in ${esc(entry.name)}</summary>${renderPage(entry.key)}</details>` : "");
      }).join("");
      if (result.more)
        html += `<details class="local-alias" data-preview-more="${esc(prefix)}"><summary>Press 9 for more folders \xB7 page ${page + 2}</summary>${renderPage(prefix, page + 1)}</details>`;
      return html;
    };
    return renderPage();
  }
  function folderPage() {
    const all = names();
    const filtered = all.filter(
      (remote) => remote.toLocaleLowerCase().includes(editor.query.toLocaleLowerCase().trim())
    );
    const pages = Math.max(1, Math.ceil(filtered.length / editor.pageSize));
    editor.page = Math.min(editor.page, pages - 1);
    const start = editor.page * editor.pageSize;
    return {
      all,
      filtered,
      pages,
      start,
      rows: filtered.slice(start, start + editor.pageSize)
    };
  }
  function folderRow(remote, index) {
    const e = editor;
    const role = Object.keys(e.roles).find(
      (key) => remoteKey(e.roles[key]) === remoteKey(remote)
    ) || "";
    const local = e.mapping[mappingName(remote)] ?? e.folders[remote];
    return `<div class="folder-row${e.selected.has(remote) ? " is-selected" : ""}">
    <input class="check" type="checkbox" data-folder-select="${esc(remote)}" id="folder-select-${index}" aria-label="Select ${esc(remote)}"${e.selected.has(remote) ? " checked" : ""}>
    <label class="folder-name" for="folder-select-${index}"><img class="folder-icon" src="/assets/${role === "sent" ? "cutout-53-e8ef5973a543.png" : "added-099-folder.svg"}" alt="" width="20" height="20"><span>${esc(remote)}<small class="description">${excluded(remote) ? "Hidden from phone" : groupFor(remote) ? `Group: ${esc(labelFor(remote))}` : "Separate"}</small></span></label>
    <label class="select-wrap"><span class="field-caption">Role</span><select data-folder-role="${esc(remote)}" aria-label="Role for ${esc(remote)}">${option("", "No role", !role)}${Object.entries(
      roleNames
    ).map(([key, name]) => option(key, name, role === key)).join("")}</select></label>
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
      ["preview", "Phone preview", "folder-tabs-phone.svg"]
    ];
    const panelAttrs = (tab) => `id="folder-panel-${tab}" role="tabpanel" aria-labelledby="folder-tab-${tab}" tabindex="0"${e.tab === tab ? "" : " hidden"}`;
    $("#folder-editor").innerHTML = `
    <header class="editor-heading"><div><h2 id="folder-editor-title" tabindex="-1">Folder mapping for ${esc(e.name)}</h2><p>Choose folders, assign roles and decide which duplicates belong together. Choices take effect when saved.</p></div></header>
    <div class="editor-tabs" role="tablist" aria-label="Folder choices">${tabs.map(([tab, label, icon]) => `<button type="button" role="tab" id="folder-tab-${tab}" data-folder-tab="${tab}" aria-controls="folder-panel-${tab}" aria-selected="${e.tab === tab}" tabindex="${e.tab === tab ? 0 : -1}"><img src="/assets/${icon}" alt="" width="18" height="18">${label}</button>`).join("")}</div>
    ${e.warnings.length ? `<div class="notice" role="status"><div><h3>Folder mapping conflicts</h3>${e.warnings.map((warning) => `<p>${esc(warning)}</p>`).join("")}<p>Choose an unused destination from each folder\u2019s dropdown, such as Separate/Sent. Existing destinations remain reserved to protect synced mail.</p></div></div>` : ""}
    <div class="editor-panels">
      <section class="panel folder-list-panel" ${panelAttrs("folders")}>
        <div class="intro"><h3>Discovered folders</h3><p>Select folders for an action. Selections stay selected across pages and searches.</p></div>
        <div class="folder-toolbar"><label class="folder-search"><span class="sr-only">Search folders</span><input type="search" id="folder-search" placeholder="Search folders\u2026" value="${esc(e.query)}"></label><div class="action-buttons"><button type="button" data-folder-action="select-page">Select this page</button><button type="button" data-folder-action="clear-selection">Clear selection</button></div></div>
        <div class="folder-table-head" aria-hidden="true"><span></span><span>Folder</span><span>Role</span><span>Destination</span><span>Phone visibility</span></div>
        <div class="folders">${page.rows.map(folderRow).join("") || `<p class="folder-empty">${page.all.length ? "No folders match your search." : "No folder inventory yet. Use Test saved settings on this account to discover its provider folders, then reopen Folder choices."}</p>`}</div>
        <div class="folder-pagination"><label>Rows per page<select id="folder-page-size">${[5, 10, 25].map((size) => option(String(size), String(size), size === e.pageSize)).join("")}</select></label><span role="status" aria-live="polite">${page.filtered.length ? `${page.start + 1}\u2013${Math.min(page.start + e.pageSize, page.filtered.length)}` : "0"} of ${page.filtered.length} folders</span><div class="page-buttons"><button type="button" data-folder-action="previous-page" aria-label="Previous page"${e.page === 0 ? " disabled" : ""}>\u2039</button><span>Page ${e.page + 1} of ${page.pages}</span><button type="button" data-folder-action="next-page" aria-label="Next page"${e.page + 1 >= page.pages ? " disabled" : ""}>\u203A</button></div></div>
        <div class="actions"><div class="selection-summary"><h3>Folder actions</h3><p id="folder-selection-count">${e.selected.size} selected</p></div><div class="action-buttons"><button type="button" data-folder-action="combine">Combine selected</button><button type="button" data-folder-action="separate">Keep separate</button><button type="button" data-folder-action="exclude">Exclude from browsing</button><button type="button" data-folder-action="include">Show selected</button></div><p class="description">Excluded folders are hidden from the phone folder menu. Their emails are retained.</p></div>
      </section>
      <section class="panel group" ${panelAttrs("groups")}><div class="intro"><h3>Mailbox groups</h3><p>Select folders in the Folders tab, then combine them here. Choose a display name and a primary folder for writes.</p></div>
        <div class="group-layout"><div><label class="group-choice">Edit group<select id="folder-group-choice">${option("", "Choose a group", !group)}${e.policy.groups.map((item) => option(item.id, item.name || "Unnamed group", item.id === e.active)).join("")}</select></label>
        <div class="group-box">${group ? `<label class="field-label" for="folder-group-name">Display name</label><input class="name-input" id="folder-group-name" value="${esc(group.name)}" maxlength="1024"><div class="members-heading"><span class="field-label">Members \xB7 choose primary</span></div><div class="member-list">${group.folders.map((remote, index) => `<div class="member"><input class="radio" type="radio" name="folder-primary" id="folder-primary-${index}" data-folder-primary="${esc(remote)}"${remoteKey(group.primary) === remoteKey(remote) ? " checked" : ""}><label for="folder-primary-${index}">${esc(remote)}</label>${remoteKey(group.primary) === remoteKey(remote) ? '<span class="primary"><img src="/assets/cutout-162-3952d56441af.png" alt="" width="16" height="16">Primary</span>' : ""}</div>`).join("")}</div><div class="action-buttons"><button type="button" data-folder-action="add-members">Add selected members</button><button type="button" data-folder-action="dissolve">Keep all members separate</button></div>` : "<p>Select at least two folders in the Folders tab and choose Combine selected to create a group. No primary is chosen automatically.</p>"}</div></div>
        <div class="roles"><h3>Folder roles</h3><p class="description">Assign roles in the Folders tab. A grouped role uses your chosen primary for writes.</p><div class="role-list">${Object.entries(
      roleNames
    ).map(
      ([role, name]) => `<div class="role-row"><strong>${esc(name)}</strong><span>${esc(e.roles[role] ? labelFor(e.roles[role]) : "Unassigned")}</span></div>`
    ).join("")}</div></div></div>
      </section>
      <section class="panel result" ${panelAttrs("preview")}><div class="intro"><h3>Resulting mailboxes</h3><p>Preview after the next successful sync. Inbox, Drafts, Sent and Trash keep keys 1\u20134. Other folders use 5\u20138; press 9 for more. Expand nested folders to see their keys.</p><p>On the phone, select a folder and press 1 to listen to messages there, or 2 to open its nested folders.</p></div><div id="folder-preview" class="mailboxes">${e.tab === "preview" ? preview() : ""}</div><div class="notice"><div><h4>Excluded folders: ${e.policy.excluded.length}</h4><p>${esc(e.policy.excluded.join(", ") || "None. Every folder is available in the phone menu.")}</p></div></div></section>
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
      root.querySelectorAll("button,input,select").forEach((input) => input.disabled = true);
  }
  function detach(remotes) {
    const keys = new Set([...remotes].map(remoteKey));
    for (const group of editor.policy.groups) {
      group.folders = group.folders.filter(
        (remote) => !keys.has(remoteKey(remote))
      );
      if (!group.folders.some(
        (remote) => remoteKey(remote) === remoteKey(group.primary)
      ))
        group.primary = "";
    }
    editor.policy.groups = editor.policy.groups.filter(
      (group) => group.folders.length > 1
    );
  }
  function onChange(event) {
    var _a, _b;
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
      input.checked ? editor.selected.add(remote) : editor.selected.delete(remote);
      input.closest(".folder-row").classList.toggle("is-selected", input.checked);
      $("#folder-selection-count").textContent = `${editor.selected.size} selected`;
      return;
    }
    if (input.hasAttribute("data-folder-visibility")) {
      const remote = input.dataset.folderVisibility;
      editor.policy.excluded = editor.policy.excluded.filter(
        (member) => remoteKey(member) !== remoteKey(remote)
      );
      if (input.value === "ignore") {
        detach(/* @__PURE__ */ new Set([remote]));
        editor.policy.excluded.push(remote);
      }
      renderEditor();
      document.querySelector(`[data-folder-visibility="${CSS.escape(remote)}"]`)?.focus();
      return;
    }
    if (input.hasAttribute("data-folder-mapping")) {
      const remote = input.dataset.folderMapping;
      editor.mapping[mappingName(remote)] = input.value;
      renderEditor();
      document.querySelector(`[data-folder-mapping="${CSS.escape(remote)}"]`)?.focus();
      message("Local mapping changed. Save when ready.");
      return;
    }
    if (input.hasAttribute("data-folder-alias")) {
      editor.mapping[mappingName(input.dataset.folderAlias)] = input.value;
      const select = document.querySelector(
        `[data-folder-mapping="${CSS.escape(input.dataset.folderAlias)}"]`
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
        (_a = editor.mapping)[_b = mappingName(remote)] ?? (_a[_b] = editor.folders[remote]);
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
    const refocus = input.id || (input.dataset.folderRole ? `[data-folder-role="${CSS.escape(input.dataset.folderRole)}"]` : "");
    renderEditor();
    if (refocus)
      (refocus.startsWith("[") ? document.querySelector(refocus) : document.getElementById(refocus))?.focus();
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
    const action = event.target.closest("[data-folder-action]")?.dataset.folderAction;
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
    if (["next-page", "previous-page", "select-page", "clear-selection"].includes(
      action
    )) {
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
    const selected = new Set(editor.selected), group = selectedGroup();
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
        (remote) => ![...selected].some(
          (member) => remoteKey(member) === remoteKey(remote)
        )
      );
    if (action === "combine") {
      const id = crypto.randomUUID();
      editor.policy.groups.push({
        id,
        name: "",
        primary: "",
        folders: [...selected]
      });
      editor.active = id;
      editor.tab = "groups";
    }
    if (action === "exclude")
      editor.policy.excluded = uniqueRemotes([
        ...editor.policy.excluded,
        ...selected
      ]);
    if (action === "add-members") {
      const id = group.id;
      const original = { ...group };
      const members = uniqueRemotes([...group.folders, ...selected]);
      detach(selected);
      editor.policy.groups = editor.policy.groups.filter(
        (item) => item.id !== id
      );
      editor.policy.groups.push({ ...original, folders: members });
      editor.active = id;
    }
    if (action === "dissolve" && group)
      editor.policy.groups = editor.policy.groups.filter(
        (item) => item.id !== group.id
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
      (item) => !item.name.trim() || !item.primary
    );
    if (group) {
      e.active = group.id;
      e.tab = "groups";
      renderEditor();
      message(
        "Each group needs a display name and an explicitly chosen primary folder.",
        true,
        true
      );
      return;
    }
    e.busy = true;
    $("#folder-editor").setAttribute("aria-busy", "true");
    $("#folder-editor").querySelectorAll("button,input,select").forEach((input) => input.disabled = true);
    message("Saving folder choices\u2026");
    try {
      const result = await api(
        `/api/v1/accounts/${encodeURIComponent(e.id)}/folder-policy`,
        {
          method: "PUT",
          body: JSON.stringify({
            config_version: e.version,
            policy: e.policy,
            folder_map: e.mapping,
            folder_roles: e.roles
          })
        }
      );
      if (e !== editor || e.epoch !== state.sessionEpoch) return;
      e.version = result.config_version;
      Object.assign(e.savedMapping, e.mapping);
      message("Folder choices saved.", false, true);
      if (e.warnings.length) {
        try {
          const snapshot = await api(
            `/api/v1/accounts/${encodeURIComponent(e.id)}/folder-policy`
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
            true
          );
        }
      }
      try {
        await e.afterSave();
      } catch (error) {
        if (e === editor && e.epoch === state.sessionEpoch)
          message(
            "Folder choices saved, but the account list could not refresh: " + error.message,
            true,
            true
          );
      }
    } catch (error) {
      if (e === editor && e.epoch === state.sessionEpoch)
        message(
          "Folder choices could not be saved: " + error.message,
          true,
          true
        );
    } finally {
      if (e === editor && e.epoch === state.sessionEpoch) {
        e.busy = false;
        $("#folder-editor").setAttribute("aria-busy", "false");
        $("#folder-editor").querySelectorAll("button,input,select").forEach((input) => input.disabled = false);
        const page = folderPage();
        $("[data-folder-action='previous-page']").disabled = e.page === 0;
        $("[data-folder-action='next-page']").disabled = e.page + 1 >= page.pages;
      }
    }
  }

  // internal/web/console/src/mailboxes.js
  function mapText(text) {
    const out = {};
    for (const line of String(text || "").split(/\r?\n/)) {
      const i = line.indexOf("=");
      if (i > 0) {
        const k = line.slice(0, i).trim(), v = line.slice(i + 1).trim();
        if (k && v) out[k] = v;
      }
    }
    return out;
  }
  function mapTextBack(v) {
    return Object.entries(v || {}).map(([k, x]) => k + "=" + x).join("\n");
  }
  function beginAccountAction() {
    if (state.accountActionBusy) return false;
    state.accountActionBusy = true;
    $("#accounts").setAttribute("aria-busy", "true");
    $("#account-form").setAttribute("aria-busy", "true");
    document.querySelectorAll("#account-form button,#accounts button").forEach((b) => b.disabled = true);
    return true;
  }
  function endAccountAction() {
    state.accountActionBusy = false;
    $("#accounts").setAttribute("aria-busy", "false");
    $("#account-form").setAttribute("aria-busy", "false");
    document.querySelectorAll("#account-form button,#accounts button").forEach((b) => b.disabled = false);
    updateSyncButtons();
  }
  function accountPayload() {
    const f = $("#account-form"), v = formData(f), roles = {};
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
        v.reconciliation_interval_minutes || 1440
      ),
      display_order: Number(v.display_order || 0),
      folder_map: mapText(v.folder_map),
      folder_roles: roles,
      alert_folders: String(v.alert_folders || "").split(",").map((x) => x.trim()).filter(Boolean),
      call_alert_enabled: f.call_alert_enabled.checked
    };
    if (v.config_version) a.config_version = Number(v.config_version);
    if (v.initial_cutoff) a.initial_cutoff = v.initial_cutoff;
    if (v.retention_days) a.retention_days = Number(v.retention_days);
    return a;
  }
  function fillAccount(a) {
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
      "retention_days"
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
    $("#account-test-help").textContent = "Test edited values uses this form without saving. Test saved settings ignores unsaved changes.";
    f.classList.remove("hidden");
    f.scrollIntoView({ behavior: "smooth", block: "start" });
    f.elements.canonical_name.focus();
  }
  function resetAccount() {
    const f = $("#account-form");
    f.reset();
    f.elements.id.value = "";
    f.elements.imap_port.value = 993;
    f.elements.smtp_port.value = 465;
    f.elements.sync_interval_minutes.value = 5;
    f.elements.reconciliation_interval_minutes.value = 1440;
    $("#test-saved-account").classList.add("hidden");
    $("#account-test-help").textContent = "Tests the current form values without saving the account.";
    $("#folder-discovery").textContent = "";
    f.classList.add("hidden");
  }
  async function saveAccount(a) {
    if (!a.id) {
      status("Testing the new mailbox values\u2026");
      const result = await api("/api/v1/accounts/validate", {
        method: "POST",
        body: JSON.stringify(a)
      });
      showFolders(result.folders || []);
    }
    return api("/api/v1/accounts", { method: "POST", body: JSON.stringify(a) });
  }
  function renderAccounts() {
    if (!state.accounts.length) {
      $("#accounts").innerHTML = "<p>No accounts configured.</p>";
      return;
    }
    const alertHead = state.alertsAvailable ? '<th class="alert-control">Alerts</th>' : "";
    $("#accounts").innerHTML = "<table><thead><tr><th>Order</th><th>Name</th><th>Address</th><th>IMAP</th><th>Sync</th>" + alertHead + "<th>Actions</th></tr></thead><tbody>" + state.accounts.map((a) => {
      const alert = state.alertsAvailable ? '<td class="alert-control" data-label="Alerts">' + (a.call_alert_enabled ? '<span class="pill">enabled</span>' : "\u2014") + "</td>" : "";
      return '<tr data-account-id="' + esc(a.id) + '"><td data-label="Order">' + esc(a.display_order) + '</td><td data-label="Name">' + esc(a.canonical_name) + '</td><td data-label="Address">' + esc(a.email) + '</td><td data-label="IMAP">' + esc(a.imap_host) + ":" + esc(a.imap_port) + '</td><td data-label="Sync" class="sync-detail" aria-live="polite" aria-atomic="true" data-sync-status="' + esc(a.id) + '">' + syncDetails(a) + "</td>" + alert + '<td data-label="Actions" class="actions"><button class="link" data-edit-account="' + esc(a.id) + '">Edit</button><button class="link" data-folders-account="' + esc(a.id) + '">Folder choices</button><button class="link" aria-label="Move ' + esc(a.canonical_name) + ' up" title="Move up" data-up-account="' + esc(a.id) + '">\u2191</button><button class="link" aria-label="Move ' + esc(a.canonical_name) + ' down" title="Move down" data-down-account="' + esc(a.id) + '">\u2193</button><button class="link" aria-label="Test saved settings for ' + esc(a.canonical_name) + '" data-test-account="' + esc(a.id) + '">Test saved settings</button><button class="link" data-sync-account="' + esc(a.id) + '">Sync</button><button class="link" data-delete-account="' + esc(a.id) + '">Delete</button></td></tr>';
    }).join("") + "</tbody></table>";
    if (state.accountActionBusy)
      document.querySelectorAll("#accounts button:not([data-cancel-sync])").forEach((button) => button.disabled = true);
    updateSyncButtons();
  }
  async function reorder(id, delta) {
    const i = state.accounts.findIndex((a) => a.id === id), j = i + delta;
    if (i < 0 || j < 0 || j >= state.accounts.length) return;
    const ids = state.accounts.map((a) => a.id);
    [ids[i], ids[j]] = [ids[j], ids[i]];
    await api("/api/v1/accounts/order", {
      method: "PUT",
      body: JSON.stringify({ ids })
    });
    await loadAccounts();
    status("Mailbox order updated.");
  }
  async function loadAccounts() {
    state.accounts = await api("/api/v1/accounts");
    renderAccounts();
  }
  function showFolders(folders) {
    $("#folder-discovery").textContent = folders && folders.length ? "Discovered remote folders: " + folders.join(", ") : "No remote folders were returned.";
  }
  function initializeMailboxes() {
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
            "Mailbox save or refresh failed: " + x.message
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
        const a = accountPayload(), result = await api("/api/v1/accounts/validate", {
          method: "POST",
          body: JSON.stringify(a)
        });
        if (epoch !== state.sessionEpoch) return;
        showFolders(result.folders || []);
        status(
          "Edited values authenticated; no changes were saved. " + (result.folders || []).length + " folders discovered."
        );
      } catch (e) {
        if (epoch === state.sessionEpoch)
          operationFeedback(
            "#account-form",
            "Edited-value test failed: " + e.message
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
          body: JSON.stringify({ account_id: id })
        });
        if (epoch !== state.sessionEpoch) return;
        showFolders(result.folders || []);
        status(
          "Saved settings authenticated; unsaved edits were ignored. " + (result.folders || []).length + " folders discovered."
        );
      } catch (e) {
        if (epoch === state.sessionEpoch)
          operationFeedback(
            "#account-form",
            "Saved-settings test failed: " + e.message
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
        const epoch2 = state.sessionEpoch;
        b.textContent = "Canceling\u2026";
        clearOperationFeedback("#accounts");
        try {
          await api("/api/v1/accounts/" + b.dataset.cancelSync + "/sync/cancel", {
            method: "POST"
          });
          if (epoch2 !== state.sessionEpoch) return;
          status("Cancellation requested. Updating mailbox status\u2026");
          await loadAccounts();
          await refreshOperationalStatus();
        } catch (x) {
          if (epoch2 === state.sessionEpoch) {
            b.textContent = "Cancel sync";
            operationFeedback(
              "#accounts",
              "Mailbox sync cancellation failed: " + x.message
            );
          }
        } finally {
          if (epoch2 === state.sessionEpoch) endMutation(b);
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
            loadAccounts
          );
        if (b.dataset.deleteAccount && confirm("Delete this account and its index?")) {
          const deletion = await api(
            "/api/v1/accounts/" + b.dataset.deleteAccount,
            {
              method: "DELETE"
            }
          );
          if (epoch !== state.sessionEpoch) return;
          await loadAccounts();
          status(
            deletion?.cleanup_pending ? "Account deleted. Stored files are being removed in the background." : "Account deleted."
          );
        }
        if (b.dataset.upAccount) await reorder(b.dataset.upAccount, -1);
        if (b.dataset.downAccount) await reorder(b.dataset.downAccount, 1);
        if (b.dataset.testAccount) {
          const result = await api("/api/v1/accounts/test", {
            method: "POST",
            body: JSON.stringify({ account_id: b.dataset.testAccount })
          });
          if (epoch !== state.sessionEpoch) return;
          showFolders(result.folders || []);
          status(
            "Saved settings authenticated; " + (result.folders || []).length + " folders discovered."
          );
        }
        if (b.dataset.syncAccount) {
          status("Synchronizing\u2026");
          try {
            await api("/api/v1/accounts/" + b.dataset.syncAccount + "/sync", {
              method: "POST"
            });
            if (epoch === state.sessionEpoch)
              status(
                "Synchronization started. You can leave this page; progress and cancellation remain available."
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
          const id = b.dataset.deleteAccount || b.dataset.testAccount || b.dataset.syncAccount, name = state.accounts.find((a) => a.id === id)?.canonical_name;
          operationFeedback(
            "#accounts",
            (name ? name + ": " : "") + "Mailbox operation failed: " + x.message
          );
        }
      } finally {
        if (epoch === state.sessionEpoch) endAccountAction();
      }
    };
  }

  // internal/web/console/src/caller-access.js
  function renderContacts() {
    const el = $("#contacts");
    el.innerHTML = state.contacts.length ? "<table><thead><tr><th>Order</th><th>Name</th><th>Email</th><th>Actions</th></tr></thead><tbody>" + state.contacts.map(
      (c) => "<tr><td>" + esc(c.display_order) + "</td><td>" + esc(c.name) + "</td><td>" + esc(c.email) + '</td><td class="actions"><button class="link" data-edit-contact="' + c.id + '">Edit</button><button class="link" aria-label="Move ' + esc(c.name) + ' up" title="Move up" data-up-contact="' + c.id + '">\u2191</button><button class="link" aria-label="Move ' + esc(c.name) + ' down" title="Move down" data-down-contact="' + c.id + '">\u2193</button><button class="link" data-delete-contact="' + c.id + '">Delete</button></td></tr>'
    ).join("") + "</tbody></table>" : "<p>No contacts.</p>";
    applyMutationPending(el);
  }
  async function reorderContact(id, delta) {
    const i = state.contacts.findIndex((c) => c.id === id), other = state.contacts[i + delta];
    if (i < 0 || !other) return;
    [state.contacts[i], state.contacts[i + delta]] = [
      state.contacts[i + delta],
      state.contacts[i]
    ];
    await Promise.all(
      state.contacts.map(
        (c, n) => api("/api/v1/contacts/" + c.id, {
          method: "PUT",
          body: JSON.stringify({
            name: c.name,
            email: c.email,
            display_order: n
          })
        })
      )
    );
    await loadContacts();
    status("Contact order updated.");
  }
  async function loadContacts() {
    state.contacts = await api("/api/v1/contacts");
    renderContacts();
  }
  async function loadPhones() {
    const rows = await api("/api/v1/whitelist");
    $("#phones").innerHTML = rows.length ? "<ul>" + rows.map(
      (x) => "<li>" + esc(x.phone) + ' <button class="link" data-delete-phone="' + x.id + '">Remove</button></li>'
    ).join("") + "</ul>" : "<p>No allowed callers.</p>";
    applyMutationPending("#phones");
  }
  function initializeCallerAccess() {
    $("#contact-form").onsubmit = async (e) => {
      e.preventDefault();
      const f = e.target;
      if (!beginMutation(f)) return;
      const epoch = state.sessionEpoch;
      clearOperationFeedback(f);
      try {
        const v = formData(f), id = v.id;
        delete v.id;
        v.display_order = Number(v.display_order || 0);
        await api(id ? "/api/v1/contacts/" + id : "/api/v1/contacts", {
          method: id ? "PUT" : "POST",
          body: JSON.stringify(v)
        });
        if (epoch !== state.sessionEpoch) return;
        f.reset();
        f.elements.id.value = "";
        await loadContacts();
        status("Contact saved.");
      } catch (x) {
        if (epoch === state.sessionEpoch)
          operationFeedback(f, "Contact save or refresh failed: " + x.message);
      } finally {
        if (epoch === state.sessionEpoch) endMutation(f);
      }
    };
    $("#cancel-contact").onclick = () => {
      const f = $("#contact-form");
      if (f.dataset.mutationPending === "true") return;
      f.reset();
      f.elements.id.value = "";
      f.elements.name.focus();
    };
    $("#contacts").onclick = async (e) => {
      const b = e.target.closest("button");
      if (!b) return;
      const id = Number(
        b.dataset.editContact || b.dataset.deleteContact || b.dataset.upContact || b.dataset.downContact
      ), c = state.contacts.find((x) => x.id === id);
      if (b.dataset.editContact) {
        if ($("#contacts").dataset.mutationPending === "true") return;
        const f = $("#contact-form");
        for (const k of ["id", "name", "email", "display_order"])
          f.elements[k].value = c[k] ?? "";
        f.scrollIntoView({ behavior: "smooth" });
        f.elements.name.focus();
        return;
      }
      if (!b.dataset.upContact && !b.dataset.downContact && !b.dataset.deleteContact)
        return;
      if (!beginMutation("#contacts")) return;
      const epoch = state.sessionEpoch;
      clearOperationFeedback("#contacts");
      try {
        if (b.dataset.upContact) await reorderContact(id, -1);
        if (b.dataset.downContact) await reorderContact(id, 1);
        if (b.dataset.deleteContact) {
          await api("/api/v1/contacts/" + id, { method: "DELETE" });
          if (epoch !== state.sessionEpoch) return;
          await loadContacts();
          status("Contact deleted.");
        }
      } catch (x) {
        if (epoch === state.sessionEpoch)
          operationFeedback("#contacts", "Contact action failed: " + x.message);
      } finally {
        if (epoch === state.sessionEpoch) endMutation("#contacts");
      }
    };
    $("#phone-form").onsubmit = async (e) => {
      e.preventDefault();
      const f = e.target;
      if (!beginMutation(f, 'button[type="submit"]')) return;
      const epoch = state.sessionEpoch;
      clearOperationFeedback(f);
      try {
        await api("/api/v1/whitelist", {
          method: "POST",
          body: JSON.stringify(formData(f))
        });
        if (epoch !== state.sessionEpoch) return;
        f.reset();
        await loadPhones();
        status("Caller allowed.");
      } catch (x) {
        if (epoch === state.sessionEpoch)
          operationFeedback(f, "Caller could not be allowed: " + x.message);
      } finally {
        if (epoch === state.sessionEpoch) endMutation(f, 'button[type="submit"]');
      }
    };
    $("#phones").onclick = async (e) => {
      const b = e.target.closest("[data-delete-phone]");
      if (!b || !beginMutation("#phones")) return;
      const epoch = state.sessionEpoch;
      clearOperationFeedback("#phones");
      try {
        await api("/api/v1/whitelist/" + b.dataset.deletePhone, {
          method: "DELETE"
        });
        if (epoch !== state.sessionEpoch) return;
        await loadPhones();
        status("Caller removed from the allow list.");
      } catch (x) {
        if (epoch === state.sessionEpoch)
          operationFeedback("#phones", "Caller removal failed: " + x.message);
      } finally {
        if (epoch === state.sessionEpoch) endMutation("#phones");
      }
    };
  }

  // internal/web/console/src/voices.js
  function applyAlertAvailability(value) {
    state.alertsAvailable = !!value;
    document.querySelectorAll(".alert-control,#alert-controls").forEach((el) => el.classList.toggle("hidden", !state.alertsAvailable));
    $("#alert-controls-extra").classList.toggle("hidden", !state.alertsAvailable);
    $("#settings-form").closest("section").querySelector("h2").textContent = state.alertsAvailable ? "Voice and call alerts" : "Voice settings";
    renderAccounts();
  }
  async function loadVoices() {
    const rows = await api("/api/v1/voices");
    $("#voice-list").innerHTML = rows.map(
      (x) => '<option value="' + esc(x.voice) + '">' + (x.installed ? "installed" : "available to administrators") + "</option>"
    ).join("");
    $("#install-voice").classList.toggle("hidden", !window.isAdmin);
    if (window.isAdmin) await loadVoiceWarm(rows);
    else $("#voice-warm-controls").classList.add("hidden");
    const field = $("#settings-form").elements.tts_voice;
    field.placeholder = window.isAdmin ? "Piper voice name or HTTPS .onnx URL" : "Installed voice name";
    field.title = window.isAdmin ? "Install any Piper voice by name or paste its HTTPS model URL. The matching .onnx.json file is downloaded with it." : "Choose an installed voice.";
  }
  async function loadSettings() {
    const v = await api("/api/v1/settings");
    applyAlertAvailability(v.alerts_available !== false);
    const f = $("#settings-form");
    f.elements.tts_voice.value = v.tts_voice || "";
    f.elements.menu_speed.value = v.menu_speed || 3;
    f.elements.email_speed.value = v.email_speed || 2;
    if (v.alerts_available !== false) {
      f.elements.alerts_enabled.checked = !!v.alerts_enabled;
      f.elements.alert_phone.value = v.alert_phone || "";
    }
  }
  async function loadAlertNumbers() {
    const rows = await api("/api/v1/alert-numbers");
    $("#alert-numbers").innerHTML = rows.length ? "<ul>" + rows.map(
      (x) => "<li>" + esc(x.number) + " " + (x.active ? '<span class="pill">active</span>' : '<button class="link" data-activate-alert="' + x.id + '">Make active</button>') + ' <button class="link" data-delete-alert="' + x.id + '">Remove</button></li>'
    ).join("") + "</ul>" : "<p>No alert numbers configured.</p>";
    applyMutationPending("#alert-numbers");
  }
  async function loadVoiceWarm(rows) {
    let warm = { resident: [], keep_warm: {} };
    try {
      warm = await api("/api/v1/voices/warm");
    } catch (x) {
      if (x && x.message && /resident speech engine/i.test(x.message)) {
        $("#voice-warm-status").textContent = "The resident speech engine is disabled (VOXMAIL_PIPER_RESIDENT=0).";
      } else {
        $("#voice-warm-status").textContent = "Voice warmth status could not be read: " + (x.message || x);
      }
      return;
    }
    const pins = warm.keep_warm || {};
    const installed = (rows || []).filter((x) => x.installed).map((x) => x.voice);
    $("#voice-warm-list").innerHTML = installed.length ? installed.map(
      (name) => '<label class="row"><input type="checkbox" data-keep-warm="' + esc(name) + '"' + (pins[name] ? " checked" : "") + "> " + esc(name) + (warm.resident && warm.resident.includes(name) ? ' <span class="pill">loaded</span>' : "") + "</label>"
    ).join("") : '<p class="muted">No voices installed.</p>';
    $("#voice-warm-status").textContent = "";
    $("#voice-warm-controls").classList.remove("hidden");
  }
  async function saveVoiceWarm(name, enabled, box) {
    const previous = !enabled;
    box.disabled = true;
    try {
      await api(
        "/api/v1/voices/warm/" + encodeURIComponent(name),
        { method: "PUT", body: JSON.stringify({ enabled }) }
      );
      await loadVoiceWarm();
      status(
        enabled ? "Voice pinned as always-warm." : "Voice unpinned; it unloads after the last call using it."
      );
    } catch (x) {
      box.checked = previous;
      status("Voice warmth change failed: " + (x.message || x));
    } finally {
      box.disabled = false;
    }
  }
  function setVoiceInstallPending(pending) {
    state.voiceInstallBusy = pending;
    $("#install-voice").disabled = pending || state.previewVoiceBusy;
    $("#preview-voice").disabled = pending || state.previewVoiceBusy;
    $("#voice-operation-controls").setAttribute(
      "aria-busy",
      String(pending || state.previewVoiceBusy)
    );
    const cancel = $("#cancel-voice-install");
    if (!pending && cancel.dataset.mutationPending === "true")
      endMutation(cancel);
    cancel.classList.toggle("hidden", !pending);
    cancel.disabled = pending && !state.activeVoiceJobID;
    if (!pending) cancel.textContent = "Cancel installation";
  }
  function finishVoicePreview(epoch) {
    if (epoch !== state.sessionEpoch) return;
    state.previewVoiceBusy = false;
    if ($("#preview-voice").dataset.mutationPending === "true")
      endMutation("#preview-voice");
    $("#preview-voice").disabled = state.voiceInstallBusy;
    $("#install-voice").disabled = state.voiceInstallBusy;
    $("#voice-operation-controls").setAttribute(
      "aria-busy",
      String(state.voiceInstallBusy)
    );
  }
  async function waitVoiceJob(id) {
    const epoch = state.sessionEpoch, signal = state.sessionAbort.signal;
    for (let n = 0; n < 1800; n++) {
      const job = await api("/api/v1/voices/jobs/" + encodeURIComponent(id));
      if (epoch !== state.sessionEpoch) throw changedSession();
      const percent = Math.max(0, Math.min(100, Number(job.progress) || 0));
      $("#voice-status").textContent = (job.stage || job.status) + " " + percent + "%";
      $("#voice-progress").value = percent;
      $("#voice-progress").classList.remove("hidden");
      const announcedStage = job.stage || job.status, announcementKey = announcedStage + ":" + Math.floor(percent / 10);
      if (!$("#voice-announcement").dataset.progressKey || $("#voice-announcement").dataset.progressKey.split(":")[0] !== announcedStage || Math.floor(percent / 10) > Number($("#voice-announcement").dataset.progressKey.split(":")[1])) {
        $("#voice-announcement").dataset.progressKey = announcementKey;
        $("#voice-announcement").textContent = announcedStage + " " + percent + " percent.";
      }
      if (job.status === "complete") {
        await loadVoices();
        $("#settings-form").elements.tts_voice.value = job.voice;
        return;
      }
      if (job.status === "failed")
        throw Error(job.error || "voice installation failed");
      await sessionDelay(1e3, signal);
    }
    throw Error("voice installation timed out");
  }
  async function runVoiceInstallJob(getJob, startingMessage) {
    if (state.voiceInstallBusy || state.previewVoiceBusy) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback("#settings-form");
    state.activeVoiceJobID = "";
    setVoiceInstallPending(true);
    $("#voice-status").textContent = startingMessage;
    $("#voice-progress").value = 0;
    $("#voice-progress").classList.remove("hidden");
    $("#voice-announcement").textContent = startingMessage;
    try {
      const job = await getJob();
      if (epoch !== state.sessionEpoch) return;
      state.activeVoiceJobID = job.job_id;
      $("#cancel-voice-install").disabled = false;
      await waitVoiceJob(job.job_id);
      if (epoch !== state.sessionEpoch) return;
      $("#voice-status").textContent = "Installed. Save this voice to use it.";
      $("#voice-progress").value = 100;
      $("#voice-announcement").dataset.progressKey = "";
      $("#voice-announcement").textContent = "Voice installation complete.";
      status("Voice installed. Save this voice setting to use it.");
    } catch (x) {
      if (epoch !== state.sessionEpoch) return;
      const canceled = x.message === "voice installation canceled";
      $("#voice-status").textContent = canceled ? "Canceled. You can retry." : x.message;
      $("#voice-progress").classList.add("hidden");
      $("#voice-progress").value = 0;
      $("#voice-announcement").dataset.progressKey = "";
      $("#voice-announcement").textContent = canceled ? "Voice installation canceled. You can retry." : "Voice installation stopped. You can retry.";
      if (canceled) status("Voice installation canceled. You can retry.");
      else
        operationFeedback(
          "#settings-form",
          "Piper voice installation failed: " + x.message
        );
    } finally {
      if (epoch === state.sessionEpoch) {
        state.activeVoiceJobID = "";
        setVoiceInstallPending(false);
      }
    }
  }
  async function resumeActiveVoiceInstall() {
    if (!window.isAdmin || state.voiceInstallBusy) return;
    const epoch = state.sessionEpoch;
    try {
      const job = await api("/api/v1/voices/jobs/active");
      if (epoch !== state.sessionEpoch || !job.job_id) return;
      await runVoiceInstallJob(async () => job, "Resuming voice installation\u2026");
    } catch (x) {
      if (epoch === state.sessionEpoch && x.name !== "AbortError")
        operationFeedback(
          "#settings-form",
          "Active Piper installation status could not be recovered: " + x.message
        );
    }
  }
  function initializeVoices() {
    document.addEventListener("change", async (e) => {
      const box = e.target;
      if (box instanceof HTMLInputElement && box.dataset.keepWarm !== void 0) {
        await saveVoiceWarm(box.dataset.keepWarm, box.checked, box);
      }
    });
    $("#settings-form").onsubmit = async (e) => {
      e.preventDefault();
      const f = e.target;
      if (!beginMutation(f, 'button[type="submit"]')) return;
      const epoch = state.sessionEpoch;
      clearOperationFeedback(f);
      try {
        const v = formData(f);
        v.menu_speed = Number(v.menu_speed);
        v.email_speed = Number(v.email_speed);
        v.alerts_enabled = f.elements.alerts_enabled.checked;
        v.alert_phone = v.alert_phone || null;
        await api("/api/v1/settings", { method: "PUT", body: JSON.stringify(v) });
        if (epoch === state.sessionEpoch)
          status("Voice and alert settings saved.");
      } catch (x) {
        if (epoch === state.sessionEpoch)
          operationFeedback(
            f,
            "Voice and alert settings could not be saved: " + x.message
          );
      } finally {
        if (epoch === state.sessionEpoch) endMutation(f, 'button[type="submit"]');
      }
    };
    $("#alert-number-form").onsubmit = async (e) => {
      e.preventDefault();
      const f = e.target;
      if (!beginMutation(f, 'button[type="submit"]')) return;
      const epoch = state.sessionEpoch;
      clearOperationFeedback(f);
      try {
        const v = formData(f);
        v.active = f.elements.active.checked;
        await api("/api/v1/alert-numbers", {
          method: "POST",
          body: JSON.stringify(v)
        });
        if (epoch !== state.sessionEpoch) return;
        f.reset();
        f.elements.active.checked = true;
        await loadAlertNumbers();
        status("Alert number saved.");
      } catch (x) {
        if (epoch === state.sessionEpoch)
          operationFeedback(f, "Alert number could not be saved: " + x.message);
      } finally {
        if (epoch === state.sessionEpoch) endMutation(f, 'button[type="submit"]');
      }
    };
    $("#alert-numbers").onclick = async (e) => {
      const b = e.target.closest("button");
      if (!b || !b.dataset.activateAlert && !b.dataset.deleteAlert || !beginMutation("#alert-numbers"))
        return;
      const epoch = state.sessionEpoch;
      clearOperationFeedback("#alert-numbers");
      try {
        if (b.dataset.activateAlert)
          await api(
            "/api/v1/alert-numbers/" + b.dataset.activateAlert + "/active",
            { method: "PUT" }
          );
        if (b.dataset.deleteAlert)
          await api("/api/v1/alert-numbers/" + b.dataset.deleteAlert, {
            method: "DELETE"
          });
        if (epoch !== state.sessionEpoch) return;
        await loadAlertNumbers();
        if (b.dataset.activateAlert) status("Alert number activated.");
        if (b.dataset.deleteAlert) status("Alert number deleted.");
      } catch (x) {
        if (epoch === state.sessionEpoch)
          operationFeedback(
            "#alert-numbers",
            "Alert number action failed: " + x.message
          );
      } finally {
        if (epoch === state.sessionEpoch) endMutation("#alert-numbers");
      }
    };
    $("#install-voice").onclick = () => runVoiceInstallJob(
      () => api("/api/v1/voices/install", {
        method: "POST",
        body: JSON.stringify({
          voice: $("#settings-form").elements.tts_voice.value
        })
      }),
      "Voice installation queued\u2026"
    );
    $("#cancel-voice-install").onclick = async () => {
      if (!state.voiceInstallBusy || !state.activeVoiceJobID || !beginMutation("#cancel-voice-install"))
        return;
      const epoch = state.sessionEpoch, id = state.activeVoiceJobID, button = $("#cancel-voice-install");
      button.textContent = "Canceling\u2026";
      $("#voice-announcement").textContent = "Voice installation cancellation requested.";
      try {
        await api("/api/v1/voices/jobs/" + encodeURIComponent(id) + "/cancel", {
          method: "POST"
        });
        if (epoch === state.sessionEpoch)
          status("Cancellation requested. The voice installer is stopping.");
      } catch (x) {
        if (epoch === state.sessionEpoch) {
          button.textContent = "Cancel installation";
          endMutation(button);
          operationFeedback(
            "#settings-form",
            "Piper voice cancellation failed: " + x.message
          );
        }
      }
    };
    $("#preview-voice").onclick = async () => {
      const button = $("#preview-voice");
      if (!beginMutation(button)) return;
      const epoch = state.sessionEpoch, f = $("#settings-form");
      state.previewVoiceBusy = true;
      $("#voice-operation-controls").setAttribute("aria-busy", "true");
      clearOperationFeedback(f);
      try {
        const job = await api("/api/v1/voices/preview", {
          method: "POST",
          body: JSON.stringify({
            voice: f.elements.tts_voice.value,
            speed: Number(f.elements.menu_speed.value || 3)
          })
        });
        if (epoch !== state.sessionEpoch) throw changedSession();
        $("#voice-status").textContent = "Generating preview\u2026";
        $("#voice-announcement").textContent = "Generating voice preview.";
        const path = "/api/v1/voices/previews/" + encodeURIComponent(job.job_id);
        for (; ; ) {
          const current = await api(path);
          if (epoch !== state.sessionEpoch) throw changedSession();
          if (current.status === "complete") break;
          if (current.status === "failed")
            throw Error(current.failure?.reason || "Voice preview failed");
          await sessionDelay(500);
        }
        const r = await fetch(path + "/audio", {
          signal: state.sessionAbort.signal
        });
        if (epoch !== state.sessionEpoch) throw changedSession();
        if (!r.ok) {
          const b = await r.json().catch(() => ({}));
          throw Error(b.error || r.statusText);
        }
        const blob = await r.blob();
        if (epoch !== state.sessionEpoch) throw changedSession();
        state.previewVoiceURL = URL.createObjectURL(blob);
        const audio = state.previewVoiceAudio = new Audio(
          state.previewVoiceURL
        );
        audio.onended = () => {
          if (state.previewVoiceURL) {
            URL.revokeObjectURL(state.previewVoiceURL);
            state.previewVoiceURL = "";
          }
          state.previewVoiceAudio = null;
          finishVoicePreview(epoch);
          if (epoch === state.sessionEpoch) {
            $("#voice-announcement").textContent = "Voice preview finished.";
            status("Voice preview finished.");
          }
        };
        audio.onerror = () => {
          if (state.previewVoiceURL) {
            URL.revokeObjectURL(state.previewVoiceURL);
            state.previewVoiceURL = "";
          }
          state.previewVoiceAudio = null;
          finishVoicePreview(epoch);
        };
        await audio.play();
        if (epoch !== state.sessionEpoch) throw changedSession();
        $("#voice-status").textContent = "Playing preview.";
        $("#voice-announcement").textContent = "Voice preview started.";
        status("Voice preview started.");
      } catch (x) {
        if (state.previewVoiceURL) {
          URL.revokeObjectURL(state.previewVoiceURL);
          state.previewVoiceURL = "";
        }
        state.previewVoiceAudio = null;
        finishVoicePreview(epoch);
        if (epoch === state.sessionEpoch) {
          $("#voice-status").textContent = "";
          operationFeedback(f, "Voice preview failed: " + x.message);
        }
      }
    };
    $("#test-alert").onclick = async () => {
      const button = $("#test-alert");
      if (!beginMutation(button)) return;
      const epoch = state.sessionEpoch;
      clearOperationFeedback(button);
      try {
        await api("/api/v1/alerts/test", { method: "POST" });
        if (epoch === state.sessionEpoch)
          status("Test alert call placed; the phone should ring.");
      } catch (x) {
        if (epoch === state.sessionEpoch)
          operationFeedback(button, "Test alert call failed: " + x.message);
      } finally {
        if (epoch === state.sessionEpoch) endMutation(button);
      }
    };
  }

  // internal/web/console/src/administration.js
  async function loadGlobalAlerts() {
    if (!window.isAdmin) return;
    const v = await api("/api/v1/admin/alerts");
    $("#admin-alert-panel").classList.remove("hidden");
    $("#global-alerts-available").checked = !!v.available;
    applyAlertAvailability(v.available);
  }
  async function loadUsers() {
    try {
      const rows = await api("/api/v1/users");
      $("#users-panel").classList.remove("hidden");
      $("#users").innerHTML = rows.length ? "<table><thead><tr><th>User</th><th>Role</th><th>Status</th><th></th></tr></thead><tbody>" + rows.map(
        (x) => "<tr><td>" + esc(x.username) + "</td><td>" + esc(x.role) + "</td><td>" + (x.enabled ? "enabled" : "disabled") + "</td><td>" + (x.id === window.userId ? "current" : '<button class="link" data-delete-user="' + esc(x.id) + '">Delete</button>') + "</td></tr>"
      ).join("") + "</tbody></table>" : "<p>No users.</p>";
      applyMutationPending("#users");
    } catch (_) {
      $("#users-panel").classList.add("hidden");
    }
  }
  async function loadSIP() {
    try {
      const v = await api("/api/v1/sip"), f = $("#sip-form");
      f.elements.enabled.checked = !!v.enabled;
      f.elements.domain.value = v.domain || "";
      f.elements.username.value = v.username || "";
      f.elements.local_port.value = v.local_port || v.port || 5060;
      f.elements.registrar_port.value = v.registrar_port || v.port || 5060;
      f.elements.transport.value = v.transport || "udp";
      f.elements.reg_interval.value = v.reg_interval == null ? 300 : v.reg_interval;
      f.elements.password.value = "";
      $("#sip-password-state").textContent = v.password_set ? "A password is stored. To change it, type a new one:" : "No password stored yet.";
      $("#sip-panel").classList.remove("hidden");
    } catch (_) {
      $("#sip-panel").classList.add("hidden");
    }
  }
  function initializeAdministration() {
    $("#save-global-alerts").onclick = async () => {
      const button = $("#save-global-alerts");
      if (!beginMutation(button)) return;
      const epoch = state.sessionEpoch;
      clearOperationFeedback(button);
      try {
        const available = $("#global-alerts-available").checked;
        await api("/api/v1/admin/alerts", {
          method: "PUT",
          body: JSON.stringify({ available })
        });
        if (epoch !== state.sessionEpoch) return;
        applyAlertAvailability(available);
        if (!available) $("#alert-numbers").innerHTML = "";
        status(
          available ? "Call alerts are available to users." : "Call alerts are disabled for all users."
        );
      } catch (x) {
        if (epoch === state.sessionEpoch)
          operationFeedback(
            button,
            "Global alert setting could not be saved: " + x.message
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
          body: JSON.stringify(formData(f))
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
          method: "DELETE"
        });
        if (epoch !== state.sessionEpoch) return;
        await loadUsers();
        status(
          deletion?.cleanup_pending ? "User deleted. Stored files are being removed in the background." : "User deleted."
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
            "SIP settings could not be saved or applied: " + x.message
          );
      } finally {
        if (epoch === state.sessionEpoch) endMutation(f, 'button[type="submit"]');
      }
    };
  }

  // internal/web/console/src/navigation.js
  var taskDetails = {
    mailboxes: [
      "Mailboxes",
      "Manage your connected mail accounts, folder mappings, and synchronization settings."
    ],
    "caller-access": [
      "Caller access",
      "Manage your email contacts and the callers allowed to access your mail by phone."
    ],
    "voice-alerts": [
      "Voice & alerts",
      "Choose your telephone voice, speaking speeds, and call-alert destinations."
    ],
    security: [
      "Account security",
      "Update your password and telephone PIN, and manage authenticator protection."
    ],
    administration: [
      "Administration",
      "Manage console users, deployment SIP settings, and global alert availability."
    ]
  };
  function renderNavigation(focus = false) {
    if (!window.userId) return;
    document.body.dataset.signedIn = "true";
    $("#admin-navigation").classList.toggle("hidden", !window.isAdmin);
    let task = location.hash.slice(1);
    if (!taskDetails[task] || task === "administration" && !window.isAdmin)
      task = "mailboxes";
    document.body.dataset.consoleTask = task;
    $("#console-wordmark").src = task === "administration" ? "/assets/console-admin-wordmark.webp" : "/assets/console-mailboxes-wordmark.webp";
    $("#admin-nav-art").classList.toggle("hidden", task !== "administration");
    if (location.hash !== "#" + task) history.replaceState(null, "", "#" + task);
    document.querySelectorAll("[data-route]").forEach((el) => el.classList.toggle("hidden", el.dataset.route !== task));
    document.querySelectorAll("[data-task]").forEach((el) => {
      if (el.dataset.task === task) el.setAttribute("aria-current", "page");
      else el.removeAttribute("aria-current");
    });
    const [title, copy] = taskDetails[task];
    $("#task-title").textContent = title;
    $("#task-copy").textContent = copy;
    document.title = title + " \xB7 VOXMail";
    if (focus) $("#task-title").focus();
  }
  function initializeNavigation() {
    window.addEventListener("hashchange", () => renderNavigation(true));
    $(".skip-link").onclick = (e) => {
      e.preventDefault();
      $("#console-content").focus();
    };
    $(".task-nav").onclick = (e) => {
      const link = e.target.closest("[data-task]");
      if (link && location.hash === link.getAttribute("href")) {
        e.preventDefault();
        renderNavigation(true);
      }
    };
  }

  // internal/web/console/src/bootstrap.js
  async function loadAll() {
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
        window.isAdmin ? loadSIP() : Promise.resolve()
      ]);
      if (window.isAdmin) void resumeActiveVoiceInstall();
    } catch (e) {
      status(e.message, true);
    }
  }
  function initializeBootstrap() {
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

  // internal/web/console/src/authentication.js
  function renderAuthState(setupAvailable) {
    const setup = !!setupAvailable;
    document.body.dataset.setupAvailable = String(setup);
    $("#auth-loading").classList.add("hidden");
    setAuthMode(setup ? "setup" : "login", false);
    $("#auth-eyebrow").textContent = setup ? "First-run setup" : "Private by design";
    $("#auth-heading").innerHTML = setup ? "Start with one secure administrator." : "Your mailbox, <br>ready when you are.";
    $("#auth-description").textContent = setup ? "Create the first administrator to unlock the console. You can add mail accounts and users after setup." : "VOXMail connects your trusted mail accounts to a simple telephone experience and a focused management console.";
  }
  var initialSetupState = document.body.dataset.setupAvailable;
  function authMessage(message2, error = false) {
    $("#auth-status").textContent = message2;
    $("#auth-status").className = "status" + (error ? " error" : "");
  }
  function authPending(pending) {
    state.authBusy = pending;
    document.querySelectorAll("#auth form").forEach((f) => {
      f.setAttribute("aria-busy", String(pending));
      f.querySelectorAll("button[type=submit]").forEach(
        (b) => b.disabled = pending
      );
    });
  }
  function setAuthMode(mode, focus = true) {
    state.authEpoch++;
    beginSession();
    state.authMode = mode;
    authPending(false);
    authMessage("");
    ["#setup", "#login", "#recovery-request", "#recovery-confirm"].forEach(
      (id) => {
        $(id).reset();
        $(id).classList.toggle("hidden", id !== "#" + mode);
      }
    );
    $("#recovery-tools").classList.toggle("hidden", mode !== "login");
    $("#recovery-back").classList.toggle("hidden", !mode.startsWith("recovery-"));
    $("#auth").dataset.state = mode;
    clearSensitiveFields();
    if (focus) {
      const field = $("#" + mode).querySelector(
        "input:not([type=hidden]):not([readonly]):not([disabled])"
      );
      if (field) field.focus();
    }
  }
  function openRecovery(purpose) {
    state.recoveryPurpose = purpose;
    setAuthMode("recovery-request");
    $("#recovery-request").elements.purpose.value = purpose;
    $("#recovery-title").textContent = purpose === "reset_password" ? "Reset password" : "Bypass TOTP";
    $("#recovery-request-copy").textContent = purpose === "reset_password" ? "Request a code through your configured mailbox to reset your password." : "Request a mailbox code to sign in once without your authenticator. Your 2FA stays enabled.";
  }
  function confirmRecovery(email) {
    setAuthMode("recovery-confirm");
    const f = $("#recovery-confirm");
    f.elements.email.value = email;
    f.elements.purpose.value = state.recoveryPurpose;
    const reset = state.recoveryPurpose === "reset_password";
    $("#recovery-confirm-title").textContent = reset ? "Choose a new password" : "Sign in without TOTP";
    $("#recovery-password").classList.toggle("hidden", !reset);
    f.elements.new_password.disabled = !reset;
    f.elements.new_password.required = reset;
    f.elements.code.focus();
    authMessage("If the address is configured, a recovery code will be sent.");
  }
  async function signIn(path, id) {
    if (state.authBusy) return;
    const epoch = state.authEpoch, payload = formData($(id));
    if (path === "/api/v1/setup") payload.sign_in = false;
    authPending(true);
    try {
      const u = await api(path, {
        method: "POST",
        body: JSON.stringify(payload)
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
  function initializeAuthentication() {
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
      const epoch = state.authEpoch, payload = formData(e.target);
      authPending(true);
      authMessage("Sending recovery request\u2026");
      try {
        await api("/api/v1/recovery/request", {
          method: "POST",
          body: JSON.stringify(payload)
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
      authMessage("Checking recovery code\u2026");
      try {
        const v = await api("/api/v1/recovery/confirm", {
          method: "POST",
          body: JSON.stringify(formData(e.target))
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
      } catch (_) {
      }
      if (epoch === state.sessionEpoch) {
        showSignedOut("");
        location.reload();
      }
    };
  }

  // internal/web/console/src/main.js
  initializeAuthentication();
  initializeOperations();
  initializeNavigation();
  initializeMailboxes();
  initializeCallerAccess();
  initializeVoices();
  initializeAdministration();
  initializeSecurity();
  initializeBootstrap();
})();
