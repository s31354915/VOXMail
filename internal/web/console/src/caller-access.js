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
  const rows = await api("/api/v1/whitelist");
  $("#phones").innerHTML = rows.length
    ? "<ul>" +
      rows
        .map(
          (x) =>
            "<li>" +
            esc(x.phone) +
            ' <button class="link" data-delete-phone="' +
            x.id +
            '">Remove</button></li>',
        )
        .join("") +
      "</ul>"
    : "<p>No allowed callers.</p>";
  applyMutationPending("#phones");
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
      await api("/api/v1/whitelist", {
        method: "POST",
        body: JSON.stringify(formData(f)),
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
