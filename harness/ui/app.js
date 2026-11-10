// Harness UI: polls the API and renders the ticket list and the selected
// ticket. No framework: the page is small enough not to need one.
const STEPS = ["readiness", "build", "ci", "review", "risk", "merge", "deploy", "smoke"];
const STEP_LABEL = { readiness: "Readiness", build: "Build", ci: "CI", review: "Review", risk: "Risk gate",
  merge: "Merge", deploy: "Deploy", smoke: "Smoke test", resume: "Resume" };
const ACTION_LABEL = { answer: "Send answer", approve: "Approve and merge", retry: "Retry with guidance",
  takeover: "Take over", resume: "Resume", abandon: "Abandon" };
const WAIT_TITLE = { questions: "The readiness check has questions", escalated: "Escalated: it needs your decision",
  approval: "Needs your approval to merge", takeover: "You've taken this over", error: "A step keeps failing" };

const $ = (id) => document.getElementById(id);
let selected = location.hash.slice(1) || null;
let shownWait = null; // which wait card is rendered, so polling doesn't wipe a half-typed comment
let shownVerdict = null; // same for the feedback card
const openDetails = new Set(); // timeline entries the user expanded, kept open across polls

async function api(path, opts) {
  const res = await fetch(path, opts && { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(opts) });
  const body = await res.json();
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

function el(tag, attrs = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") n.className = v; else if (k.startsWith("on")) n.addEventListener(k.slice(2), v); else n.setAttribute(k, v);
  }
  n.append(...children.filter((c) => c != null && c !== false));
  return n;
}

const dotClass = (t) => (t.status === "waiting" && t.wait === "error" ? "error" : t.status);
const ago = (iso) => {
  const s = Math.round((Date.now() - new Date(iso)) / 1000);
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  return new Date(iso).toLocaleString();
};
const statusText = (t) => {
  if (t.status === "waiting") return "Needs you";
  if (t.status === "done") return "Deployed";
  if (t.status === "abandoned") return "Abandoned";
  return `${STEP_LABEL[t.step] || t.step}${t.status === "running" ? "…" : ""}`;
};

async function refreshList() {
  const [tickets, stats] = await Promise.all([api("/api/tickets"), api("/api/stats")]);
  $("improve-count").textContent = stats.openProposals || "";
  $("improve-link").classList.toggle("active", selected === "improvements");
  $("tickets").replaceChildren(...tickets.map((t) => el("a", { href: `#${t.id}`, class: t.id === selected ? "active" : "" },
    el("div", { class: "t-title" }, el("span", { class: `dot ${dotClass(t)}` }), t.title),
    el("div", { class: `t-sub ${t.status === "waiting" ? "needs" : ""}` }, `${t.id} · ${statusText(t)}`))));
}

async function refreshImprovements() {
  const [stats, proposals] = await Promise.all([api("/api/stats"), api("/api/proposals")]);
  const stat = (v, label) => el("div", { class: "stat" }, el("b", {}, v), el("span", {}, label));
  $("stats").replaceChildren(
    stat(`${stats.deployed}/${stats.finished}`, "tickets deployed"),
    stat(stats.rated ? `${stats.wanted}/${stats.rated}` : "–", "did what you wanted (of those rated)"),
    stat(stats.avgQuestions.toFixed(1), "questions asked per ticket"),
    stat(stats.avgBuilds.toFixed(1), "build rounds per ticket"),
    stat(`$${stats.avgCostUSD.toFixed(2)}`, "agent spend per ticket"));
  if (!proposals.length) {
    return $("proposals").replaceChildren(el("p", { class: "muted" }, "No proposals yet. They appear here after tickets finish."));
  }
  $("proposals").replaceChildren(...proposals.map((p) => {
    const err = el("span", { class: "error" });
    const decide = (action) => async () => {
      try { await api(`/api/proposals/${p.id}`, { action }); tick(); } catch (e) { err.textContent = e.message; tick(); }
    };
    return el("div", { class: `proposal ${p.status}` },
      el("h3", {}, `${p.id}: change the ${p.step} prompt`, el("span", { class: "state" }, p.status),
        el("a", { href: `#${p.ticket}`, class: "muted" }, `from ${p.ticket}`)),
      el("p", { class: "why" }, p.why),
      el("div", { class: "diff" }, el("div", { class: "del" }, `- ${p.find}`), el("div", { class: "add" }, `+ ${p.replace}`)),
      p.status === "open" ? el("div", { class: "row" },
        el("button", { class: "primary", onclick: decide("apply") }, "Apply"),
        el("button", { onclick: decide("dismiss") }, "Dismiss"), err) : null);
  }));
}

async function refreshTicket() {
  if (!selected) return show("empty");
  if (selected === "improvements") { show("improvements"); return refreshImprovements(); }
  let data;
  try { data = await api(`/api/tickets/${selected}`); } catch { return show("empty"); }
  const { ticket: t, events, actions } = data;
  show("ticket");
  $("t-id").textContent = t.id;
  $("t-title").textContent = t.title;
  const pill = $("t-status");
  pill.textContent = statusText(t);
  pill.className = `pill ${t.status}`;
  $("t-desc").textContent = t.description || "(no description)";

  const meta = [];
  if (t.branch) meta.push(el("span", { class: "mono" }, t.branch));
  if (t.builds) meta.push(el("span", {}, `${t.builds} build${t.builds > 1 ? "s" : ""}`));
  if (t.risk) meta.push(el("span", {}, `risk ${t.risk}`));
  meta.push(el("span", {}, `$${t.costUSD.toFixed(2)} agent spend`));
  if (t.status === "done") meta.push(el("a", { href: "/poster", target: "_blank" }, "Open the deployed poster ↗"));
  $("t-meta").replaceChildren(...meta);

  const cur = STEPS.indexOf(t.step === "resume" ? "ci" : t.step);
  $("t-steps").replaceChildren(...STEPS.map((s, i) => {
    let cls = i < cur || t.status === "done" ? "done" : "";
    if (i === cur && t.status !== "done") cls = `current ${t.status === "waiting" ? "waiting" : ""}`;
    const label = s === "build" && t.round ? `${STEP_LABEL[s]} · round ${t.round}` : STEP_LABEL[s];
    return el("li", { class: cls }, label);
  }));

  renderNeeds(t, actions || []);
  renderVerdict(t);
  renderAssumptions(t);

  $("t-events").replaceChildren(...events.map((e) => el("li", { class: e.kind },
    el("div", { class: "ev-head" },
      el("span", { class: "ev-step" }, STEP_LABEL[e.step] || e.step),
      el("span", {}, e.message),
      e.costUSD ? el("span", { class: "ev-cost" }, `$${e.costUSD.toFixed(3)}`) : null,
      el("span", { class: "ev-time", title: new Date(e.at).toLocaleString() }, ago(e.at))),
    e.detail ? details(e) : null)));
}

// The timeline is rebuilt on every poll, so remember which entries are open.
function details(e) {
  const d = el("details", { class: "ev-detail" }, el("summary", {}, "Details"), el("pre", {}, e.detail));
  d.open = openDetails.has(e.id);
  d.addEventListener("toggle", () => (d.open ? openDetails.add(e.id) : openDetails.delete(e.id)));
  return d;
}

function renderAssumptions(t) {
  const as = t.assumptions || [];
  if (!as.length) return $("t-assumptions").replaceChildren();
  $("t-assumptions").replaceChildren(el("div", { class: "assumptions" },
    el("h3", {}, "Decisions made for you"),
    el("ul", {}, ...as.map((a) => el("li", {}, el("span", { class: "from" }, STEP_LABEL[a.step] || a.step), a.text)))));
}

function renderVerdict(t) {
  const finished = t.status === "done" || t.status === "abandoned";
  const key = finished ? `${t.id}:${t.verdict || ""}:${t.retro || ""}` : null;
  if (key === shownVerdict) return;
  shownVerdict = key;
  if (!finished) return $("t-verdict").replaceChildren();
  const retro = { pending: "The retro will look at it shortly.", running: "The retro is looking at it now.",
    done: "The retro has looked at it.", failed: "The retro failed; see the timeline." }[t.retro] || "";
  if (t.verdict) {
    return $("t-verdict").replaceChildren(el("div", { class: "verdict" },
      el("p", { class: "said" }, `You said it ${t.verdict === "yes" ? "did" : "didn't do"} what you wanted`,
        t.verdictComment ? `: "${t.verdictComment}"` : "", `. ${retro}`)));
  }
  const comment = el("textarea", { rows: 2, placeholder: "Optional: what was missing or wrong?" });
  const err = el("span", { class: "error" });
  const send = (good) => async () => {
    try { await api(`/api/tickets/${t.id}/feedback`, { good, comment: comment.value }); shownVerdict = null; tick(); }
    catch (e) { err.textContent = e.message; }
  };
  $("t-verdict").replaceChildren(el("div", { class: "verdict" },
    el("h3", {}, "Did this do what you wanted?"),
    comment,
    el("div", { class: "row" },
      el("button", { onclick: send(true) }, "👍 Yes"),
      el("button", { onclick: send(false) }, "👎 No"),
      el("span", { class: "muted" }, "Optional. It's the best evidence the retro gets."), err)));
}

function renderNeeds(t, actions) {
  const key = actions.length ? `${t.id}:${t.wait}:${t.updated}` : null;
  if (key === shownWait) return;
  shownWait = key;
  if (!key) return $("t-needs").replaceChildren();
  const comment = el("textarea", { rows: 3, placeholder: t.wait === "questions" ? "Your answers" : "Optional guidance for the builder" });
  const err = el("span", { class: "error" });
  const needsComment = actions.some((a) => a === "answer" || a === "retry");
  const buttons = actions.map((a) => el("button", { class: a === actions[0] ? "primary" : "", onclick: async () => {
    if (a === "abandon" && !confirm(`Abandon ${t.id}?`)) return;
    try { await api(`/api/tickets/${t.id}/actions`, { action: a, comment: comment.value }); shownWait = null; tick(); }
    catch (e) { err.textContent = e.message; }
  } }, a === "retry" && t.wait === "error" ? "Retry step" : ACTION_LABEL[a]));
  $("t-needs").replaceChildren(el("div", { class: `needs ${t.wait === "error" ? "error-wait" : ""}` },
    el("h3", {}, WAIT_TITLE[t.wait] || "Needs you"),
    el("pre", {}, t.waitText || ""),
    needsComment ? comment : null,
    el("div", { class: "row" }, ...buttons, err)));
}

function show(id) {
  for (const s of ["create", "ticket", "improvements", "empty"]) $(s).hidden = s !== id;
}

async function tick() {
  try { await Promise.all([refreshList(), $("create").hidden ? refreshTicket() : null]); } catch (e) { console.error(e); }
}

$("new-ticket").addEventListener("click", () => { selected = null; history.replaceState(null, "", "#"); show("create"); refreshList(); $("create-form").elements.title.focus(); });
$("create-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  try {
    // f.elements, not f.title: a form's .title is its own title attribute.
    const t = await api("/api/tickets", { title: f.elements.title.value, description: f.elements.description.value });
    f.reset();
    $("create-error").textContent = "";
    location.hash = t.id;
  } catch (e) { $("create-error").textContent = e.message; }
});
window.addEventListener("hashchange", () => {
  selected = location.hash.slice(1) || null;
  shownWait = null;
  shownVerdict = null;
  if (selected) show(selected === "improvements" ? "improvements" : "ticket");
  tick();
});

tick();
setInterval(tick, 2000);
