// operations: task behavior and its event bindings.
import { state } from "./state.js";
import { esc, $, api } from "./core.js";

export function timestamp(value) {
  const date = new Date(value);
  return value && Number.isFinite(date.getTime())
    ? '<time datetime="' +
        esc(value) +
        '" title="' +
        esc(date.toISOString()) +
        '">' +
        esc(date.toLocaleString()) +
        "</time>"
    : "Unknown time";
}

export function failureDetails(f) {
  return f
    ? '<div class="operational-failure"><p><img class="failure-icon" src="/assets/console-mailboxes-failure-icon.webp" alt="" width="18" height="18"> ' +
        esc(f.reason) +
        "</p><p>" +
        esc(f.action) +
        "</p>" +
        (f.diagnostic ? '<p class="muted">' + esc(f.diagnostic) + "</p>" : "") +
        "</div>"
    : "";
}

export function syncDetails(a) {
  const run = a.last_sync;
  if (!run) return '<span class="sync-state">Never synchronized</span>';
  const state =
      run.state ||
      (run.finished_at ? (run.success ? "succeeded" : "failed") : "unfinished"),
    names = {
      running: "Synchronizing…",
      unfinished: "Unfinished sync — worker not confirmed",
      succeeded: "Synchronized",
      failed: "Sync failed",
    },
    last = a.last_successful_sync || (run.success ? run : null),
    progress =
      state === "running"
        ? '<progress aria-label="Mailbox synchronization in progress"></progress>'
        : "";
  const cancel =
    state === "running" && run.kind === "manual"
      ? '<button type="button" class="link" data-cancel-sync="' +
        esc(a.id) +
        '">Cancel sync</button>'
      : "";
  return (
    '<span class="sync-state">' +
    esc(names[state] || "Unknown sync state") +
    "</span>" +
    progress +
    cancel +
    (state === "running"
      ? "<p>Mail is still synchronizing. You can cancel this manual run and retry later.</p>"
      : "") +
    "<p>Started: " +
    timestamp(run.started_at) +
    "</p>" +
    (run.finished_at
      ? "<p>Finished: " + timestamp(run.finished_at) + "</p>"
      : "") +
    "<p>Last successful sync: " +
    (last ? timestamp(last.finished_at) : "Never") +
    "</p>" +
    failureDetails(run.failure) +
    '<p class="muted">Run ' +
    esc(run.id) +
    " · " +
    esc(run.kind) +
    "</p>"
  );
}

export function renderRuntime(snapshot) {
  const names = {
    ready: "Ready",
    not_ready: "Not ready",
    registered: "Registered",
    registering: "Registering…",
    failed: "Failed",
    unregistered: "Unregistered",
    unknown: "Unknown",
    disabled: "Disabled",
    starting: "Starting…",
    running: "Running",
    stopping: "Stopping…",
    stopped: "Stopped",
    degraded: "Degraded",
    connected: "Connected",
    disconnected: "Disconnected",
    unavailable: "Unavailable",
    not_required: "Not required",
  };
  const runtime = snapshot.runtime || {};
  for (const key of ["process", "bridge", "registration", "media"])
    $("#runtime-" + key).textContent = names[runtime[key]] || "Unknown";
  $("#runtime-readiness").textContent = snapshot.offline_mode
    ? "Offline / maintenance"
    : names[snapshot.readiness] || "Unknown";
  $("#operational-updated").innerHTML =
    "Last checked: " +
    timestamp(snapshot.checked_at) +
    (snapshot.offline_mode ? " · Provider operations are disabled." : "");
  const sipFailure = $("#sip-failure"),
    failureHTML = failureDetails(snapshot.sip_failure);
  if (sipFailure.innerHTML !== failureHTML) sipFailure.innerHTML = failureHTML;
  sipFailure.classList.toggle("hidden", !snapshot.sip_failure);
  const cleanup = snapshot.cleanup || {};
  const cleanupMessages = [];
  if (cleanup.pending > 0)
    cleanupMessages.push(
      cleanup.pending + " stored-file cleanup job(s) pending.",
    );
  if (cleanup.failed > 0)
    cleanupMessages.push(
      cleanup.failed +
        " stored-file cleanup job(s) failed. Check storage permissions and server logs; cleanup retries automatically after five minutes.",
    );
  $("#operational-status").textContent = [
    snapshot.reason || "",
    ...cleanupMessages,
  ]
    .filter(Boolean)
    .join(" ");
  $("#operational-status").classList.toggle(
    "error",
    snapshot.readiness === "not_ready" || cleanup.failed > 0,
  );
}

export function refreshOperationalStatus(announce = false) {
  if (!window.userId) return Promise.resolve();
  if (state.operationalBusy) {
    const queuedEpoch = state.sessionEpoch,
      current = state.operationalRequest;
    if (!announce || !current) return current || Promise.resolve();
    const retry = () =>
      queuedEpoch === state.sessionEpoch && window.userId
        ? refreshOperationalStatus(true)
        : undefined;
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
        api("/api/v1/accounts"),
      ]);
      if (epoch !== state.sessionEpoch) return;
      const [service, mail] = results;
      const serviceOK =
        service.status === "fulfilled" &&
        service.value &&
        service.value.runtime;
      const mailOK = mail.status === "fulfilled" && Array.isArray(mail.value);
      if (serviceOK) {
        renderRuntime(service.value);
      } else {
        const output = $("#operational-status");
        output.textContent =
          "Service status could not be refreshed. The last displayed snapshot may be stale. Use Refresh status to retry.";
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
          } else if (
            el.textContent !==
            "This account is no longer available. Reload the account list."
          )
            el.textContent =
              "This account is no longer available. Reload the account list.";
        });
        updateSyncButtons();
        const changes = [];
        for (const account of mail.value) {
          const run = account.last_sync,
            key = run
              ? `${run.id}:${run.state}:${run.failure?.category || ""}:${run.finished_at || ""}`
              : "never";
          const previous = state.syncSeenStates.get(account.id);
          if (previous !== undefined && previous !== key)
            changes.push(
              `${account.canonical_name}: ${run?.state === "failed" ? "synchronization failed" : run?.state === "succeeded" ? "synchronization completed" : run?.state === "running" ? "synchronization started" : "synchronization state changed"}.`,
            );
          state.syncSeenStates.set(account.id, key);
        }
        if (changes.length)
          $("#sync-change-announcement").textContent = changes.join(" ");
        $("#sync-freshness").textContent =
          "Sync status checked " +
          new Date().toLocaleTimeString() +
          ". Updates automatically every 10 seconds.";
        $("#sync-freshness").classList.remove("sync-stale");
      } else {
        $("#sync-freshness").textContent =
          "Sync status could not be refreshed. Displayed sync information may be stale. Use Refresh status to retry.";
        $("#sync-freshness").classList.add("sync-stale");
      }
      if (announce) {
        const output = $("#refresh-status-announcement");
        output.textContent =
          serviceOK && mailOK
            ? "Service status and mailbox sync status refreshed."
            : serviceOK
              ? "Service status refreshed; mailbox sync status could not be refreshed."
              : mailOK
                ? "Mailbox sync status refreshed; service status could not be refreshed."
                : "Status could not be refreshed. Previous details may be stale.";
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

export function scheduleOperationalPoll() {
  clearTimeout(state.operationalTimer);
  const epoch = state.sessionEpoch;
  state.operationalTimer = setTimeout(async () => {
    if (epoch !== state.sessionEpoch || !window.userId) return;
    if (!document.hidden) await refreshOperationalStatus();
    if (epoch === state.sessionEpoch && window.userId)
      scheduleOperationalPoll();
  }, 10000);
}

export function initializeOperations() {
  $("#refresh-status").onclick = async () => {
    const epoch = state.sessionEpoch;
    await refreshOperationalStatus(true);
    if (epoch === state.sessionEpoch && window.userId)
      scheduleOperationalPoll();
  };
}

export function updateSyncButtons() {
  document.querySelectorAll("[data-sync-account]").forEach((button) => {
    const account = state.accounts.find(
      (a) => a.id === button.dataset.syncAccount,
    );
    button.disabled =
      state.accountActionBusy || account?.last_sync?.state === "running";
  });
}
