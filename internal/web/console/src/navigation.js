// navigation: task behavior and its event bindings.
import { $ } from "./core.js";

export const taskDetails = {
  mailboxes: [
    "Mailboxes",
    "Manage your connected mail accounts, folder mappings, and synchronization settings.",
  ],
  "caller-access": [
    "Caller access",
    "Manage your email contacts and the callers allowed to access your mail by phone.",
  ],
  "voice-alerts": [
    "Voice & alerts",
    "Choose your telephone voice, speaking speeds, and call-alert destinations.",
  ],
  security: [
    "Account security",
    "Update your password and telephone PIN, and manage authenticator protection.",
  ],
  administration: [
    "Administration",
    "Manage console users, deployment SIP settings, and global alert availability.",
  ],
};

export function renderNavigation(focus = false) {
  if (!window.userId) return;
  document.body.dataset.signedIn = "true";
  $("#admin-navigation").classList.toggle("hidden", !window.isAdmin);
  let task = location.hash.slice(1);
  if (!taskDetails[task] || (task === "administration" && !window.isAdmin))
    task = "mailboxes";
  document.body.dataset.consoleTask = task;
  $("#console-wordmark").src =
    task === "administration"
      ? "/assets/console-admin-wordmark.webp"
      : "/assets/console-mailboxes-wordmark.webp";
  $("#admin-nav-art").classList.toggle("hidden", task !== "administration");
  if (location.hash !== "#" + task) history.replaceState(null, "", "#" + task);
  document
    .querySelectorAll("[data-route]")
    .forEach((el) => el.classList.toggle("hidden", el.dataset.route !== task));
  document.querySelectorAll("[data-task]").forEach((el) => {
    if (el.dataset.task === task) el.setAttribute("aria-current", "page");
    else el.removeAttribute("aria-current");
  });
  const [title, copy] = taskDetails[task];
  $("#task-title").textContent = title;
  $("#task-copy").textContent = copy;
  document.title = title + " · VOXMail";
  if (focus) $("#task-title").focus();
}

export function initializeNavigation() {
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
