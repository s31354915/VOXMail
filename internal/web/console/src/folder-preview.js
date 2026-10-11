// Mirrors the phone's hierarchy and fixed-role compatibility rules. Different
// provider identities are combined only through the owner's explicit policy.
const fixed = ["inbox", "drafts", "sent", "trash"];
export const remoteKey = (name) =>
  name.toUpperCase() === "INBOX" ? "INBOX" : name;
const normalize = (name) =>
  name.startsWith("/groups/")
    ? name
    : name
        .trim()
        .replaceAll("\\", "/")
        .replace(/^\/+|\/+$/g, "");
const compare = (a, b) => {
  const left = Array.from(a),
    right = Array.from(b);
  for (let i = 0; i < Math.min(left.length, right.length); i++) {
    const difference = left[i].codePointAt(0) - right[i].codePointAt(0);
    if (difference) return difference;
  }
  return left.length - right.length;
};

export function folderPreviewModel({
  folders,
  mapping,
  roles,
  policy,
  mailboxes = [],
  bindings = {},
}) {
  const excluded = new Set((policy.excluded || []).map(remoteKey));
  const groups = policy.groups || [];
  const entries = [],
    byRemote = new Map(),
    physical = new Map();
  const localFor = (remote) => {
    const alias = Object.entries(mapping).find(
      ([name]) => remoteKey(name) === remoteKey(remote),
    )?.[1];
    return normalize(
      alias || (remoteKey(remote) === "INBOX" ? "Inbox" : remote),
    )
      .split("/")
      .filter((part) => part && part !== ".")
      .join("/");
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
      members: group.folders.map(localFor),
    });
  }
  for (const remote of Object.keys(folders)) {
    if (excluded.has(remoteKey(remote)) || byRemote.has(remoteKey(remote)))
      continue;
    const local = localFor(remote);
    add({
      key: local,
      name: local,
      folders: [remote],
      primary: remote,
      members: [local],
    });
  }
  // Keep unmapped legacy cache folders present in the actual phone inventory.
  // Saved groups are rebuilt above from provider identities, never inferred.
  for (const mailbox of mailboxes) {
    if (
      mailbox.key.startsWith("/groups/") ||
      byRemote.has(remoteKey(mailbox.primary_remote)) ||
      excluded.has(remoteKey(mailbox.primary_remote))
    )
      continue;
    if (
      Object.keys(folders).some(
        (remote) => remoteKey(remote) === remoteKey(mailbox.primary_remote),
      )
    )
      continue;
    add({
      key: mailbox.key,
      name: mailbox.name,
      folders: [mailbox.primary_remote],
      primary: mailbox.primary_remote,
      members: mailbox.members,
    });
  }
  // The phone's legacy fallback sees retained indexed aliases too. Resolve
  // their exact source binding rather than guessing equivalence from names.
  for (const [local, remote] of Object.entries(bindings)) {
    const entry = byRemote.get(remoteKey(remote));
    if (entry) physical.set(normalize(local), entry);
  }
  const roleEntries = fixed.map((role) => {
    if (roles[role]) return byRemote.get(remoteKey(roles[role])) || null;
    const conventional = Object.keys(mapping).filter(
      (remote) => remote.trim().toLowerCase() === role,
    );
    if (conventional.length > 1) return null;
    if (conventional.length === 1) {
      const candidate = byRemote.get(remoteKey(conventional[0]));
      if (candidate) return candidate;
      if (role === "inbox") return null;
    }
    if (role === "inbox") return physical.get("Inbox") || null;
    const matches = [...physical].filter(
      ([local]) => local.toLowerCase() === role,
    );
    return matches.length === 1 ? matches[0][1] : null;
  });
  return { entries, roleEntries };
}

export function folderPreviewPage(model, prefix = "", page = 0) {
  const labels = new Map(model.entries.map((entry) => [entry.key, entry]));
  const children = new Set();
  prefix = normalize(prefix);
  for (const entry of model.entries) {
    const folder = normalize(entry.key);
    if (folder.startsWith("/groups/")) {
      if (!prefix) children.add(folder);
      continue;
    }
    if (
      !folder ||
      folder === prefix ||
      (prefix && !folder.startsWith(prefix + "/"))
    )
      continue;
    const rest = prefix ? folder.slice(prefix.length + 1) : folder;
    const child = rest.split("/")[0];
    children.add(prefix ? prefix + "/" + child : child);
  }
  const assigned = new Set(
    model.roleEntries.filter(Boolean).map((entry) => normalize(entry.key)),
  );
  const others = [...children]
    .filter((key) => prefix || !assigned.has(key))
    .map(
      (key) => labels.get(key) || { key, name: key, folders: [], members: [] },
    )
    .sort((a, b) => compare(a.name, b.name) || compare(a.key, b.key));
  const start = prefix ? page * 8 : page === 0 ? 0 : 4 + (page - 1) * 8;
  const limit = !prefix && page === 0 ? 4 : 8;
  const keyStart = !prefix && page === 0 ? 5 : 1;
  const rows = others
    .slice(start, start + limit)
    .map((entry, index) => ({ key: keyStart + index, entry }));
  if (!prefix && page === 0)
    rows.unshift(
      ...model.roleEntries.map((entry, index) => ({
        key: index + 1,
        role: fixed[index],
        entry,
      })),
    );
  return { rows, more: start + limit < others.length };
}
