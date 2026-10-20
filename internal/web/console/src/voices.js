// voices: task behavior and its event bindings.
import { state } from "./state.js";
import {
  $,
  esc,
  api,
  applyMutationPending,
  status,
  operationFeedback,
  clearOperationFeedback,
  beginMutation,
  endMutation,
  formData,
  changedSession,
  sessionDelay,
} from "./core.js";
import { renderAccounts } from "./mailboxes.js";

export function applyAlertAvailability(value) {
  state.alertsAvailable = !!value;
  document
    .querySelectorAll(".alert-control,#alert-controls")
    .forEach((el) => el.classList.toggle("hidden", !state.alertsAvailable));
  $("#alert-controls-extra").classList.toggle("hidden", !state.alertsAvailable);
  $("#settings-form").closest("section").querySelector("h2").textContent =
    state.alertsAvailable ? "Voice and call alerts" : "Voice settings";
  renderAccounts();
}

export async function loadVoices() {
  const rows = await api("/api/v1/voices");
  $("#voice-list").innerHTML = rows
    .map(
      (x) =>
        '<option value="' +
        esc(x.voice) +
        '">' +
        (x.installed ? "installed" : "available to administrators") +
        "</option>",
    )
    .join("");
  $("#install-voice").classList.toggle("hidden", !window.isAdmin);
  if (window.isAdmin) await loadVoiceWarm(rows);
  else $("#voice-warm-controls").classList.add("hidden");
  const field = $("#settings-form").elements.tts_voice;
  field.placeholder = window.isAdmin
    ? "Piper voice name or HTTPS .onnx URL"
    : "Installed voice name";
  field.title = window.isAdmin
    ? "Install any Piper voice by name or paste its HTTPS model URL. The matching .onnx.json file is downloaded with it."
    : "Choose an installed voice.";
}

export async function loadSettings() {
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

export async function loadAlertNumbers() {
  const rows = await api("/api/v1/alert-numbers");
  $("#alert-numbers").innerHTML = rows.length
    ? "<ul>" +
      rows
        .map(
          (x) =>
            "<li>" +
            esc(x.number) +
            " " +
            (x.active
              ? '<span class="pill">active</span>'
              : '<button class="link" data-activate-alert="' +
                x.id +
                '">Make active</button>') +
            ' <button class="link" data-delete-alert="' +
            x.id +
            '">Remove</button></li>',
        )
        .join("") +
      "</ul>"
    : "<p>No alert numbers configured.</p>";
  applyMutationPending("#alert-numbers");
}


// loadVoiceWarm renders the administrator's always-warm voice controls: one
// checkbox per installed voice plus the list of models currently loaded.
async function loadVoiceWarm(rows) {
  let warm = { resident: [], keep_warm: {} };
  try {
    warm = await api("/api/v1/voices/warm");
  } catch (x) {
    if (x && x.message && /resident speech engine/i.test(x.message)) {
      $("#voice-warm-status").textContent =
        "The resident speech engine is disabled (VOXMAIL_PIPER_RESIDENT=0).";
    } else {
      $("#voice-warm-status").textContent =
        "Voice warmth status could not be read: " + (x.message || x);
    }
    return;
  }
  const pins = warm.keep_warm || {};
  const installed = (rows || []).filter((x) => x.installed).map((x) => x.voice);
  $("#voice-warm-list").innerHTML = installed.length
    ? installed
        .map(
          (name) =>
            '<label class="row"><input type="checkbox" data-keep-warm="' +
            esc(name) +
            '"' +
            (pins[name] ? " checked" : "") +
            "> " +
            esc(name) +
            (warm.resident && warm.resident.includes(name)
              ? ' <span class="pill">loaded</span>'
              : "") +
            "</label>",
        )
        .join("")
    : '<p class="muted">No voices installed.</p>';
  $("#voice-warm-status").textContent = "";
  $("#voice-warm-controls").classList.remove("hidden");
}

async function saveVoiceWarm(name, enabled, box) {
  const previous = !enabled;
  box.disabled = true;
  try {
    await api(
      "/api/v1/voices/warm/" + encodeURIComponent(name),
      { method: "PUT", body: JSON.stringify({ enabled }) },
    );
    await loadVoiceWarm();
    status(
      enabled
        ? "Voice pinned as always-warm."
        : "Voice unpinned; it unloads after the last call using it.",
    );
  } catch (x) {
    box.checked = previous;
    status("Voice warmth change failed: " + (x.message || x));
  } finally {
    box.disabled = false;
  }
}

export function setVoiceInstallPending(pending) {
  state.voiceInstallBusy = pending;
  $("#install-voice").disabled = pending || state.previewVoiceBusy;
  $("#preview-voice").disabled = pending || state.previewVoiceBusy;
  $("#voice-operation-controls").setAttribute(
    "aria-busy",
    String(pending || state.previewVoiceBusy),
  );
  const cancel = $("#cancel-voice-install");
  if (!pending && cancel.dataset.mutationPending === "true")
    endMutation(cancel);
  cancel.classList.toggle("hidden", !pending);
  cancel.disabled = pending && !state.activeVoiceJobID;
  if (!pending) cancel.textContent = "Cancel installation";
}

export function finishVoicePreview(epoch) {
  if (epoch !== state.sessionEpoch) return;
  state.previewVoiceBusy = false;
  if ($("#preview-voice").dataset.mutationPending === "true")
    endMutation("#preview-voice");
  $("#preview-voice").disabled = state.voiceInstallBusy;
  $("#install-voice").disabled = state.voiceInstallBusy;
  $("#voice-operation-controls").setAttribute(
    "aria-busy",
    String(state.voiceInstallBusy),
  );
}

export async function waitVoiceJob(id) {
  const epoch = state.sessionEpoch,
    signal = state.sessionAbort.signal;
  for (let n = 0; n < 1800; n++) {
    const job = await api("/api/v1/voices/jobs/" + encodeURIComponent(id));
    if (epoch !== state.sessionEpoch) throw changedSession();
    const percent = Math.max(0, Math.min(100, Number(job.progress) || 0));
    $("#voice-status").textContent =
      (job.stage || job.status) + " " + percent + "%";
    $("#voice-progress").value = percent;
    $("#voice-progress").classList.remove("hidden");
    const announcedStage = job.stage || job.status,
      announcementKey = announcedStage + ":" + Math.floor(percent / 10);
    if (
      !$("#voice-announcement").dataset.progressKey ||
      $("#voice-announcement").dataset.progressKey.split(":")[0] !==
        announcedStage ||
      Math.floor(percent / 10) >
        Number($("#voice-announcement").dataset.progressKey.split(":")[1])
    ) {
      $("#voice-announcement").dataset.progressKey = announcementKey;
      $("#voice-announcement").textContent =
        announcedStage + " " + percent + " percent.";
    }
    if (job.status === "complete") {
      await loadVoices();
      $("#settings-form").elements.tts_voice.value = job.voice;
      return;
    }
    if (job.status === "failed")
      throw Error(job.error || "voice installation failed");
    await sessionDelay(1000, signal);
  }
  throw Error("voice installation timed out");
}

export async function runVoiceInstallJob(getJob, startingMessage) {
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
    $("#voice-status").textContent = canceled
      ? "Canceled. You can retry."
      : x.message;
    $("#voice-progress").classList.add("hidden");
    $("#voice-progress").value = 0;
    $("#voice-announcement").dataset.progressKey = "";
    $("#voice-announcement").textContent = canceled
      ? "Voice installation canceled. You can retry."
      : "Voice installation stopped. You can retry.";
    if (canceled) status("Voice installation canceled. You can retry.");
    else
      operationFeedback(
        "#settings-form",
        "Piper voice installation failed: " + x.message,
      );
  } finally {
    if (epoch === state.sessionEpoch) {
      state.activeVoiceJobID = "";
      setVoiceInstallPending(false);
    }
  }
}

export async function resumeActiveVoiceInstall() {
  if (!window.isAdmin || state.voiceInstallBusy) return;
  const epoch = state.sessionEpoch;
  try {
    const job = await api("/api/v1/voices/jobs/active");
    if (epoch !== state.sessionEpoch || !job.job_id) return;
    await runVoiceInstallJob(async () => job, "Resuming voice installation…");
  } catch (x) {
    if (epoch === state.sessionEpoch && x.name !== "AbortError")
      operationFeedback(
        "#settings-form",
        "Active Piper installation status could not be recovered: " + x.message,
      );
  }
}

export function initializeVoices() {
  document.addEventListener("change", async (e) => {
    const box = e.target;
    if (box instanceof HTMLInputElement && box.dataset.keepWarm !== undefined) {
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
          "Voice and alert settings could not be saved: " + x.message,
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
        body: JSON.stringify(v),
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
    if (
      !b ||
      (!b.dataset.activateAlert && !b.dataset.deleteAlert) ||
      !beginMutation("#alert-numbers")
    )
      return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback("#alert-numbers");
    try {
      if (b.dataset.activateAlert)
        await api(
          "/api/v1/alert-numbers/" + b.dataset.activateAlert + "/active",
          { method: "PUT" },
        );
      if (b.dataset.deleteAlert)
        await api("/api/v1/alert-numbers/" + b.dataset.deleteAlert, {
          method: "DELETE",
        });
      if (epoch !== state.sessionEpoch) return;
      await loadAlertNumbers();
      if (b.dataset.activateAlert) status("Alert number activated.");
      if (b.dataset.deleteAlert) status("Alert number deleted.");
    } catch (x) {
      if (epoch === state.sessionEpoch)
        operationFeedback(
          "#alert-numbers",
          "Alert number action failed: " + x.message,
        );
    } finally {
      if (epoch === state.sessionEpoch) endMutation("#alert-numbers");
    }
  };
  $("#install-voice").onclick = () =>
    runVoiceInstallJob(
      () =>
        api("/api/v1/voices/install", {
          method: "POST",
          body: JSON.stringify({
            voice: $("#settings-form").elements.tts_voice.value,
          }),
        }),
      "Voice installation queued…",
    );
  $("#cancel-voice-install").onclick = async () => {
    if (
      !state.voiceInstallBusy ||
      !state.activeVoiceJobID ||
      !beginMutation("#cancel-voice-install")
    )
      return;
    const epoch = state.sessionEpoch,
      id = state.activeVoiceJobID,
      button = $("#cancel-voice-install");
    button.textContent = "Canceling…";
    $("#voice-announcement").textContent =
      "Voice installation cancellation requested.";
    try {
      await api("/api/v1/voices/jobs/" + encodeURIComponent(id) + "/cancel", {
        method: "POST",
      });
      if (epoch === state.sessionEpoch)
        status("Cancellation requested. The voice installer is stopping.");
    } catch (x) {
      if (epoch === state.sessionEpoch) {
        button.textContent = "Cancel installation";
        endMutation(button);
        operationFeedback(
          "#settings-form",
          "Piper voice cancellation failed: " + x.message,
        );
      }
    }
  };
  $("#preview-voice").onclick = async () => {
    const button = $("#preview-voice");
    if (!beginMutation(button)) return;
    const epoch = state.sessionEpoch,
      f = $("#settings-form");
    state.previewVoiceBusy = true;
    $("#voice-operation-controls").setAttribute("aria-busy", "true");
    clearOperationFeedback(f);
    try {
      const job = await api("/api/v1/voices/preview", {
        method: "POST",
        body: JSON.stringify({
          voice: f.elements.tts_voice.value,
          speed: Number(f.elements.menu_speed.value || 3),
        }),
      });
      if (epoch !== state.sessionEpoch) throw changedSession();
      $("#voice-status").textContent = "Generating preview…";
      $("#voice-announcement").textContent = "Generating voice preview.";
      const path = "/api/v1/voices/previews/" + encodeURIComponent(job.job_id);
      for (;;) {
        const current = await api(path);
        if (epoch !== state.sessionEpoch) throw changedSession();
        if (current.status === "complete") break;
        if (current.status === "failed")
          throw Error(current.failure?.reason || "Voice preview failed");
        await sessionDelay(500);
      }
      const r = await fetch(path + "/audio", {
        signal: state.sessionAbort.signal,
      });
      if (epoch !== state.sessionEpoch) throw changedSession();
      if (!r.ok) {
        const b = await r.json().catch(() => ({}));
        throw Error(b.error || r.statusText);
      }
      const blob = await r.blob();
      if (epoch !== state.sessionEpoch) throw changedSession();
      state.previewVoiceURL = URL.createObjectURL(blob);
      const audio = (state.previewVoiceAudio = new Audio(
        state.previewVoiceURL,
      ));
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
