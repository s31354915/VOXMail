// caller-access: task behavior and its event bindings.
import { state } from "./state.js";
import {
  $,
  esc,
  applyMutationPending,
  status,
  api,
  operationFeedback,
  clearOperationFeedback,
  beginMutation,
  endMutation,
  formData,
} from "./core.js";

export function renderContacts() {
  const el = $("#contacts");
  el.innerHTML = state.contacts.length
    ? "<table><thead><tr><th>Order</th><th>Name</th><th>Email</th><th>Actions</th></tr></thead><tbody>" +
      state.contacts
        .map(
          (c) =>
            "<tr><td>" +
            esc(c.display_order) +
            "</td><td>" +
            esc(c.name) +
            "</td><td>" +
            esc(c.email) +
            '</td><td class="actions"><button class="link" data-edit-contact="' +
            c.id +
            '">Edit</button><button class="link" aria-label="Move ' +
            esc(c.name) +
            ' up" title="Move up" data-up-contact="' +
            c.id +
            '">↑</button><button class="link" aria-label="Move ' +
            esc(c.name) +
            ' down" title="Move down" data-down-contact="' +
            c.id +
            '">↓</button><button class="link" data-delete-contact="' +
            c.id +
            '">Delete</button></td></tr>',
        )
        .join("") +
      "</tbody></table>"
    : "<p>No contacts.</p>";
  applyMutationPending(el);
}

export async function reorderContact(id, delta) {
  const i = state.contacts.findIndex((c) => c.id === id),
    other = state.contacts[i + delta];
  if (i < 0 || !other) return;
  [state.contacts[i], state.contacts[i + delta]] = [
    state.contacts[i + delta],
    state.contacts[i],
  ];
  await Promise.all(
    state.contacts.map((c, n) =>
      api("/api/v1/contacts/" + c.id, {
        method: "PUT",
        body: JSON.stringify({
          name: c.name,
          email: c.email,
          display_order: n,
        }),
      }),
    ),
  );
  await loadContacts();
  status("Contact order updated.");
}

export async function loadContacts() {
  state.contacts = await api("/api/v1/contacts");
  renderContacts();
}

export async function loadPhones() {
  if (!state.accounts.length) state.accounts = await api("/api/v1/accounts");
  populateAccountSelect($("#phone-form")?.elements.account_id, state.accounts);
  const rows = await api("/api/v1/whitelist");
  $("#phones").innerHTML = rows.length
    ? "<ul>" +
      rows
        .map((x) => {
          const names = (x.accounts || [])
            .map((a) => esc(a.account_name))
            .join(", ");
          return (
            "<li>" +
            esc(x.phone) +
            (x.verified
              ? ""
              : ' <span class="muted">unverified</span>') +
            (names ? " → " + names : "") +
            ' <button class="link" data-delete-phone="' +
            x.id +
            '">Remove</button></li>'
          );
        })
        .join("") +
      "</ul>"
    : "<p>No allowed callers.</p>";
  applyMutationPending("#phones");
}

function populateAccountSelect(select, accounts) {
  if (!select) return;
  const current = select.value;
  select.innerHTML = accounts
    .map((a) => '<option value="' + esc(a.id) + '">' + esc(a.canonical_name || a.name || a.id) + "</option>")
    .join("");
  if (current && accounts.some((a) => a.id === current)) select.value = current;
}

// The verification-method selector only appears when the administrator left
// both call directions open; a 400 asking for a method reveals it on demand.
function maybeShowMethodChoice(error) {
  if (!/choose a verification method/i.test(error?.message || "")) return false;
  const label = $("#verification-method-label");
  if (label) {
    label.classList.remove("hidden");
    label.querySelector("select")?.focus();
  }
  return true;
}

export function initializeCallerAccess() {
  $("#contact-form").onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    if (!beginMutation(f)) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback(f);
    try {
      const v = formData(f),
        id = v.id;
      delete v.id;
      v.display_order = Number(v.display_order || 0);
      await api(id ? "/api/v1/contacts/" + id : "/api/v1/contacts", {
        method: id ? "PUT" : "POST",
        body: JSON.stringify(v),
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
        b.dataset.editContact ||
          b.dataset.deleteContact ||
          b.dataset.upContact ||
          b.dataset.downContact,
      ),
      c = state.contacts.find((x) => x.id === id);
    if (b.dataset.editContact) {
      if ($("#contacts").dataset.mutationPending === "true") return;
      const f = $("#contact-form");
      for (const k of ["id", "name", "email", "display_order"])
        f.elements[k].value = c[k] ?? "";
      f.scrollIntoView({ behavior: "smooth" });
      f.elements.name.focus();
      return;
    }
    if (
      !b.dataset.upContact &&
      !b.dataset.downContact &&
      !b.dataset.deleteContact
    )
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
      const v = formData(f);
      if (!f.elements.method || f.elements.method.closest(".hidden")) delete v.method;
      const result = await api("/api/v1/whitelist", {
        method: "POST",
        body: JSON.stringify(v),
      });
      if (epoch !== state.sessionEpoch) return;
      state.pendingVerification = { phone: v.phone, account_id: v.account_id };
      const note = $("#phone-verification-note");
      $("#phone-verification").classList.remove("hidden");
      if (result.status === "calling") {
        note.textContent =
          "VOXMail is calling " + v.phone + " with a code. Enter it below when the call arrives.";
      } else {
        note.textContent =
          "From " + v.phone + ", call " + (result.dial_in || "the VOXMail line") +
          " and listen for the code, then enter it below.";
      }
      f.elements.phone.value = "";
      await loadPhones();
      status(result.status === "calling" ? "Verification call placed." : "Call in to hear your code.");
    } catch (x) {
      if (epoch === state.sessionEpoch) {
        if (maybeShowMethodChoice(x))
          operationFeedback(f, "Pick how this number should be verified.");
        else operationFeedback(f, "Caller could not be added: " + x.message);
      }
    } finally {
      if (epoch === state.sessionEpoch) endMutation(f, 'button[type="submit"]');
    }
  };
  $("#phone-verify-form").onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    if (!beginMutation(f, 'button[type="submit"]')) return;
    const epoch = state.sessionEpoch;
    clearOperationFeedback(f);
    try {
      const pending = state.pendingVerification || {};
      await api("/api/v1/whitelist/verify", {
        method: "POST",
        body: JSON.stringify({
          phone: pending.phone,
          code: f.elements.code.value.trim(),
          account_id: pending.account_id,
        }),
      });
      if (epoch !== state.sessionEpoch) return;
      state.pendingVerification = null;
      f.reset();
      $("#phone-verification").classList.add("hidden");
      await loadPhones();
      status("Number verified.");
    } catch (x) {
      if (epoch === state.sessionEpoch)
        operationFeedback(f, "That code did not verify: " + x.message);
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
        method: "DELETE",
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
