"use strict";
const $ = (id) => document.getElementById(id);
const el = (tag, text) => {
  const n = document.createElement(tag);
  if (text !== undefined) n.textContent = text;
  return n;
};
const state = {
  session: 0,
  eventsGeneration: 0,
  token: "",
  config: null,
  dirty: false,
  before: "",
  offset: 0,
  more: false,
  review: null,
  detailGeneration: 0,
  timer: null,
};
const json = (value) => JSON.stringify(value, null, 2);
const stamp = (value) =>
  value ? new Date(value).toLocaleString() : "Not observed";
// Notices float over the page, so "Draft saved" is visible from the bottom of a
// long editor. Failures persist until dismissed or replaced. Routine results
// clear on navigation so they cannot be misread as describing the next page.
function notice(text, tone = "info") {
  $("notice").textContent = text;
  $("notice-bar").dataset.tone = tone;
  $("notice-bar").hidden = !text;
}
const fail = (text) => notice(text, "error");
$("notice-close").addEventListener("click", () => notice(""));
async function api(path, body) {
  const session = state.session;
  const controller = new AbortController(),
    timer = setTimeout(
      () => controller.abort(),
      path.endsWith("/activate")
        ? 180000
        : path.endsWith("/test-model")
          ? 45000
          : 20000,
    );
  try {
    const response = await fetch(path, {
      method: body === undefined ? "GET" : "POST",
      cache: "no-store",
      signal: controller.signal,
      headers: {
        Authorization: `Bearer ${state.token}`,
        ...(body === undefined
          ? {}
          : { "Content-Type": "application/json", "X-Mrmr-Control": "1" }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (session !== state.session) throw Error("Management session changed.");
    if (response.status === 401) {
      lock();
      throw Error("Operator token rejected. Unlock again.");
    }
    if (!response.ok) throw Error((await response.text()).slice(0, 1000));
    const value = await response.json();
    if (session !== state.session) throw Error("Management session changed.");
    return value;
  } finally {
    clearTimeout(timer);
  }
}
function action(id, fn) {
  $(id).addEventListener("click", async (e) => {
    e.preventDefault();
    const b = e.currentTarget;
    b.disabled = true;
    try {
      await fn();
    } catch (err) {
      fail(
        err.name === "AbortError"
          ? "Request timed out. Refresh runtime state before retrying a change."
          : err.message,
      );
    } finally {
      b.disabled = false;
      if (id === "setup-start") refreshSetupStart();
    }
  });
}
function lock() {
  state.session++;
  state.token = "";
  state.detailGeneration++;
  clearTimeout(state.timer);
  $("app").hidden = true;
  $("login").hidden = false;
  $("logout").hidden = true;
  document.body.classList.add("locked");
  $("token").value = "";
  $("yaml").value = "";
  $("review-id").value = "";
  $("host").textContent = "Management";
  state.config = null;
  state.review = null;
  state.dirty = false;
  for (const id of [
    "model-form",
    "source-form",
    "policy-form",
    "review-form",
    "search",
  ])
    $(id).reset();
  for (const id of [
    "events",
    "event-detail",
    "review-evidence",
    "review-history",
    "audit",
    "evaluations",
    "revisions",
    "diff-content",
    "configured-sources",
    "configured-models",
    "configured-rules",
    "interpreter-model",
    "connections",
    "source-health",
    "model-health",
    "overview-content",
    "active-monitors",
    // Added with the Overview and Review redesign. Both hold event text, so they
    // must be cleared with the rest of the private content on Lock.
    "attention",
    "review-queue",
    "category-options",
  ])
    $(id).replaceChildren();
  $("overview-title").textContent = "Overview";
  $("overview-sentence").textContent = "";
  $("freshness").textContent = "";
  resetSetup();
  notice("Management locked.");
}
function page() {
  const requested = location.hash.slice(1);
  const name = [
    "overview",
    "activity",
    "review",
    "configure",
    "runtime",
  ].includes(requested)
    ? requested
    : "overview";
  for (const section of document.querySelectorAll("[data-page]"))
    section.hidden = section.dataset.page !== name;
  for (const a of document.querySelectorAll("nav a")) {
    if (a.hash === `#${name}`) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
  document.title = `mrmr · ${name}`;
  if ($("notice-bar").dataset.tone !== "error") notice("");
}
// Reports used to stay blank until a Refresh click, which read as "nothing here".
// Load each page's records on arrival instead.
function loadPage() {
  if (!state.token) return;
  const name = location.hash.slice(1);
  const load =
    name === "runtime"
      ? audit
      : name === "review"
        ? () => Promise.all([reviewQueue(), evaluations()])
        : name === "overview" || !name
          ? overview
          : null;
  load?.().catch((e) => fail(e.message));
}
window.addEventListener("hashchange", () => {
  page();
  loadPage();
});
page();
$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  state.session++;
  state.token = $("token").value;
  $("token").value = "";
  try {
    await loadConfig();
    $("login").hidden = true;
    $("app").hidden = false;
    $("logout").hidden = false;
    document.body.classList.remove("locked");
    await overview();
    await events();
    const id = new URLSearchParams(location.search).get("event");
    if (id) await detail(id);
    notice("");
    loadPage();
  } catch (err) {
    fail(err.message);
  }
});
$("logout").addEventListener("click", lock);
function record(title, text) {
  const n = el("div");
  n.className = "record";
  n.append(el("h3", title), el("p", text));
  return n;
}
function table(headers, className = "") {
  const wrap = el("div"),
    t = el("table"),
    head = el("thead"),
    row = el("tr"),
    body = el("tbody");
  wrap.className = "table-wrap";
  t.className = className;
  for (const h of headers) {
    const th = el("th", h);
    th.scope = "col";
    row.append(th);
  }
  head.append(row);
  t.append(head, body);
  wrap.append(t);
  return { wrap, body };
}
function cell(text, className) {
  const n = el("td", text);
  if (className) n.className = className;
  return n;
}
// Opening an event from any page lands on Activity with its detail open, so Back
// returns to where the operator was reading.
function eventLink(e, label) {
  const a = el("a", label);
  const p = new URLSearchParams(location.search);
  p.set("event", e.id);
  a.href = `?${p}#activity`;
  a.dataset.keepId = `event:${e.id}`;
  a.addEventListener("click", (evt) => {
    if (evt.ctrlKey || evt.metaKey || evt.shiftKey) return;
    evt.preventDefault();
    history.pushState(null, "", a.href);
    page();
    detail(e.id).catch((err) => fail(err.message));
  });
  return a;
}
function showView(view, query = "") {
  $("view").value = view;
  $("query").value = query;
  state.before = "";
  state.offset = 0;
  location.hash = "activity";
  saveSearch();
  events().catch((e) => fail(e.message));
}
function renderAttention(rows) {
  const flagged = rows.filter(Signal.needsAttention).slice(0, 6);
  Signal.keepFocus(() => {
    $("attention").replaceChildren();
    if (!flagged.length) {
      const quiet = el("p", "Nothing in the latest events needs attention.");
      quiet.className = "panel help";
      $("attention").append(quiet);
      return;
    }
    const { wrap, body } = table(
      ["Received", "Service", "Message", "Outcome"],
      "records attention-table",
    );
    for (const e of flagged) {
      const row = el("tr"),
        subject = cell(undefined, "cell-subject"),
        tag = cell();
      subject.append(eventLink(e, e.subject || e.type));
      tag.append(Signal.outcomeTag(e));
      row.append(
        cell(Signal.clock(e.ingested_at), "cell-time"),
        subject,
        cell(e.message, "cell-message"),
        tag,
      );
      body.append(row);
    }
    $("attention").append(wrap);
  });
}
function renderStored(s) {
  const line = el("p");
  line.className = "stored-line";
  for (const [count, label, view] of [
    [s.events, "stored events", "all"],
    [s.unlabeled, "unlabeled", "unlabeled"],
    [s.pending, "unfinished intents", "pending"],
  ]) {
    const a = el("a");
    a.append(el("strong", count), document.createTextNode(` ${label}`));
    a.href = `?view=${view}#activity`;
    a.addEventListener("click", (e) => {
      if (e.ctrlKey || e.metaKey || e.shiftKey) return;
      e.preventDefault();
      showView(view);
    });
    line.append(a);
  }
  const errors = el("a", "Inspect errors");
  errors.href = "?view=errors#activity";
  errors.addEventListener("click", (e) => {
    if (e.ctrlKey || e.metaKey || e.shiftKey) return;
    e.preventDefault();
    showView("errors");
  });
  line.append(errors);
  $("overview-content").replaceChildren(line);
}
function renderModels(s) {
  $("model-health").replaceChildren();
  if (!s.models.length) {
    const none = el("p", "No model decisions recorded yet.");
    none.className = "panel help";
    $("model-health").append(none);
    return;
  }
  const { wrap, body } = table(
    ["Interpreter", "Model", "Successful", "Median", "Errors", "Latest decision"],
    "models-table",
  );
  for (const m of s.models) {
    const row = el("tr");
    row.append(
      cell(m.interpreter, "cell-subject"),
      cell(m.model),
      cell(`${m.successes} / ${m.count}`),
      cell(m.median_ms == null ? "Not measured" : `${m.median_ms} ms`),
      cell(m.errors),
      cell(stamp(m.last_at)),
    );
    body.append(row);
  }
  $("model-health").append(wrap);
}
function renderGate(blocked) {
  // Off is a configuration fact, not evidence of health, so it is never green.
  $("gate-lamp").dataset.state = blocked ? "stop" : "set";
  $("gate-title").textContent = blocked
    ? "Safety gate on: all effects blocked"
    : "Safety gate off: effects follow their rules";
  $("shadow").disabled = blocked;
}
async function overview() {
  clearTimeout(state.timer);
  if (!state.token) return;
  try {
    if (!state.config || state.config.setup) {
      $("overview-title").textContent = "Setup required";
      $("overview-sentence").textContent =
        "Collection is disabled until a valid configuration is activated.";
      const go = el("a", "Open Configure to set a model endpoint and source");
      go.href = "#configure";
      $("overview-content").replaceChildren(go);
      return;
    }
    const data = await api("/dashboard/api/overview");
    const reading = Signal.assess(data);
    $("host").textContent = data.hostname;
    $("overview-title").textContent = reading.title;
    $("overview-title").dataset.tone = reading.tone;
    $("overview-sentence").textContent = reading.sentence;
    $("freshness").textContent = `Runtime answered ${stamp(data.at)}`;
    $("freshness").title =
      "Source evidence resets when the runtime revision changes.";
    Signal.renderTracks($("source-health"), reading.tracks, {
      href: (name) => `?q=${encodeURIComponent(name)}#activity`,
      go: (name) => showView("all", name),
    });
    if (!data.sources.length)
      $("source-health").append(
        el("p", "No pull sources configured. HTTP ingress remains available."),
      );
    renderStored(data.summary);
    renderModels(data.summary);
    renderGate(data.effects_blocked);
    // Attention is drawn from the newest page of events and only fetched while
    // Overview is on screen, so the ten-second poll stays cheap elsewhere.
    if (["", "#overview"].includes(location.hash)) {
      const latest = await api("/dashboard/api/events?view=all&offset=0");
      renderAttention(latest.events);
    }
  } catch (err) {
    // Lock rejects in-flight requests on purpose. That is not a runtime failure,
    // and writing here would repopulate a page Lock just cleared.
    if (!state.token) return;
    $("overview-title").textContent = "The runtime is not answering";
    $("overview-title").dataset.tone = "stop";
    $("overview-sentence").textContent =
      "These tracks show the last evidence received and may be stale.";
    $("freshness").textContent = "Status refresh failed.";
    fail(err.message);
  } finally {
    if (state.token && !document.hidden)
      state.timer = setTimeout(overview, 10000);
  }
}
action("refresh-overview", overview);
document.addEventListener("visibilitychange", () => {
  clearTimeout(state.timer);
  if (!document.hidden && state.token) overview();
  else
    $("freshness").textContent =
      "Refresh paused while this page is hidden; displayed evidence may be stale.";
});
function clearDetail() {
  state.detailGeneration++;
  $("event-detail").hidden = true;
}
function saveSearch() {
  clearDetail();
  const p = new URLSearchParams();
  if ($("query").value.trim()) p.set("q", $("query").value.trim());
  if ($("view").value !== "all") p.set("view", $("view").value);
  if (state.offset) p.set("offset", state.offset);
  if (state.before) p.set("before", state.before);
  history.pushState(null, "", `${location.pathname}?${p}#activity`);
}
function restoreSearch() {
  const p = new URLSearchParams(location.search);
  state.before = p.get("before") || "";
  if (state.before && !Number.isFinite(Date.parse(state.before)))
    state.before = "";
  $("query").value = (p.get("q") || "").slice(0, 200);
  $("view").value = [
    "all",
    "errors",
    "pending",
    "unlabeled",
    "shadow",
    "ignore",
  ].includes(p.get("view"))
    ? p.get("view")
    : "all";
  state.offset = Math.max(0, Math.min(100000, Number(p.get("offset")) || 0));
}
restoreSearch();
async function events() {
  if (!state.token || state.config?.setup) return;
  const generation = ++state.eventsGeneration;
  const params = new URLSearchParams({
    q: $("query").value.trim(),
    view: $("view").value,
    offset: state.offset,
    before: state.before,
  });
  const data = await api(`/dashboard/api/events?${params}`);
  if (generation !== state.eventsGeneration) return;
  state.before = data.before || "";
  const currentURL = new URL(location.href);
  currentURL.searchParams.set("before", state.before);
  history.replaceState(null, "", currentURL);
  state.more = data.more;
  const { wrap, body } = table(
    ["Received", "Source", "Event", "Outcome"],
    "records events-table",
  );
  const selected = new URLSearchParams(location.search).get("event");
  for (const e of data.events) {
    const row = el("tr"),
      main = cell(undefined, "cell-event"),
      tag = cell();
    row.dataset.eventId = e.id;
    if (e.id === selected) row.className = "selected";
    const a = eventLink(e, e.subject || e.type);
    a.className = "cell-subject";
    const message = el("span", e.message);
    message.className = "cell-message";
    main.append(a, message);
    tag.append(Signal.outcomeTag(e));
    row.append(
      cell(Signal.clock(e.ingested_at), "cell-time"),
      cell(`${e.source} · ${e.type}`, "cell-source"),
      main,
      tag,
    );
    body.append(row);
  }
  $("events").replaceChildren(wrap);
  if (!data.events.length) {
    const none = cell(
      "No matching events. Check source scope and poll evidence.",
      "empty",
    );
    none.colSpan = 4;
    const row = el("tr");
    row.append(none);
    body.append(row);
  }
  $("previous").disabled = state.offset === 0;
  $("next").disabled = !data.more || state.offset >= 100000;
  $("page-info").textContent = data.events.length
    ? `${state.offset + 1}–${state.offset + data.events.length}`
    : "No results";
}
$("view").addEventListener("change", () =>
  $("search").requestSubmit(),
);
$("search").addEventListener("submit", (e) => {
  e.preventDefault();
  state.before = "";
  state.offset = 0;
  saveSearch();
  events().catch((e) => fail(e.message));
});
action("previous", async () => {
  state.offset = Math.max(0, state.offset - 50);
  saveSearch();
  await events();
});
action("next", async () => {
  state.offset = Math.min(100000, state.offset + 50);
  saveSearch();
  await events();
});
async function detail(id) {
  const generation = ++state.detailGeneration;
  const data = await api(`/dashboard/api/event?id=${encodeURIComponent(id)}`);
  if (generation !== state.detailGeneration || !state.token) return;
  const root = $("event-detail"),
    e = data.event;
  for (const row of document.querySelectorAll("#events tr[data-event-id]"))
    row.classList.toggle("selected", row.dataset.eventId === id);
  root.hidden = false;
  const close = el("button", "Close event");
  close.id = "close-event";
  close.addEventListener("click", () => {
    const url = new URL(location.href);
    url.searchParams.delete("event");
    history.pushState(null, "", url);
    clearDetail();
    for (const row of document.querySelectorAll("#events tr.selected"))
      row.classList.remove("selected");
    document.querySelector("#events a")?.focus();
  });
  const review = el("button", "Review this event");
  review.id = "review-this-event";
  review.className = "primary";
  review.addEventListener("click", () => {
    $("review-id").value = id;
    location.hash = "review";
    loadReview().catch((err) => fail(err.message));
  });
  const head = el("div"),
    tools = el("div");
  head.className = "detail-head";
  tools.className = "row";
  tools.append(review, close);
  const title = el("h2", e.subject || e.type);
  head.append(title, tools);
  const when = el("p", `${e.source} · ${e.type} · ${stamp(e.timestamp)}`);
  when.className = "help";
  root.replaceChildren(head, when);
  // The track answers "how far did this event get?" before any detail does.
  root.append(Signal.stageList(Signal.eventStages(data)));
  if (typeof e.data?.message === "string") {
    const message = el("p", e.data.message);
    message.className = "detail-message";
    root.append(message);
  }
  for (const x of data.executions || [])
    if (x.status === "pending") {
      const warn = el(
        "p",
        "Unfinished intent: the effect may or may not have occurred.",
      );
      warn.className = "warning";
      root.append(warn);
    } else if (x.error) {
      const err = el("pre", x.error);
      root.append(err);
    }
  for (const d of data.decisions || []) {
    root.append(el("h3", `What ${d.interpreter} answered`));
    if (d.result) root.append(Signal.answers(d.result));
    else root.append(el("p", d.error || "No valid model result."));
  }
  const parent = e.provenance?.parent_event_id;
  for (const related of [parent, ...(data.child_event_ids || [])].filter(
    Boolean,
  )) {
    const p = el("p"),
      label = `${related === parent ? "Parent" : "Child"}: ${related}`;
    p.append(eventLink({ id: related }, label));
    root.append(p);
  }
  // The server formats event_json before numbers enter JavaScript's float64, so
  // this collapsed view is the exact record, including very large identifiers.
  const raw = el("details");
  raw.append(
    el("summary", "Recorded data: normalized Event, decisions, executions, trace"),
    el("h3", "Normalized Event"),
    el("pre", data.event_json),
    el("h3", "Decisions and executions"),
    el("pre", json({ decisions: data.decisions, executions: data.executions })),
    el("h3", "Trace"),
    el("pre", json(data.trace || [])),
  );
  root.append(raw);
  root.focus({ preventScroll: true });
  // Beside the list the pane is already in view; scrolling would lose the row
  // the operator just chose. Stacked on a phone it must be brought to them.
  if (matchMedia("(max-width: 1099px)").matches)
    root.scrollIntoView({ block: "start" });
  else root.scrollTop = 0;
}
window.addEventListener("popstate", () => {
  restoreSearch();
  page();
  if (state.token) {
    events().catch((e) => fail(e.message));
    const id = new URLSearchParams(location.search).get("event");
    if (id) detail(id).catch((e) => fail(e.message));
    else {
      $("event-detail").hidden = true;
      state.detailGeneration++;
    }
  }
});
// A list of what is waiting, so labelling does not start with copying an ID.
async function reviewQueue() {
  const data = await api("/dashboard/api/events?view=unlabeled&offset=0");
  const rows = data.events.slice(0, 8);
  $("review-queue").replaceChildren();
  if (!rows.length) {
    const done = el("p", "Every stored event has a label.");
    done.className = "panel help";
    $("review-queue").append(done);
    return;
  }
  const { wrap, body } = table(
    ["Received", "Service", "Message", "Outcome"],
    "records attention-table",
  );
  for (const e of rows) {
    const row = el("tr"),
      subject = cell(undefined, "cell-subject"),
      tag = cell(),
      a = el("a", e.subject || e.type);
    a.href = "#review";
    a.addEventListener("click", (evt) => {
      evt.preventDefault();
      $("review-id").value = e.id;
      loadReview()
        .then(() => $("review-evidence").focus())
        .catch((err) => fail(err.message));
    });
    subject.append(a);
    tag.append(Signal.outcomeTag(e));
    row.append(
      cell(Signal.clock(e.ingested_at), "cell-time"),
      subject,
      cell(e.message, "cell-message"),
      tag,
    );
    body.append(row);
  }
  $("review-queue").append(wrap);
}
async function loadReview() {
  const id = $("review-id").value.trim();
  const [review, evidence] = await Promise.all([
    api(`/manage/api/review?id=${encodeURIComponent(id)}`),
    api(`/dashboard/api/event?id=${encodeURIComponent(id)}`),
  ]);
  state.review = { id, ...review };
  // Evidence sits beside the form: the operator should not hold the event in
  // memory while answering questions about it.
  const panel = el("div");
  panel.className = "panel";
  panel.append(el("h2", evidence.event.subject || evidence.event.type));
  panel.append(Signal.stageList(Signal.eventStages(evidence)));
  if (typeof evidence.event.data?.message === "string") {
    const message = el("p", evidence.event.data.message);
    message.className = "detail-message";
    panel.append(message);
  }
  for (const d of evidence.decisions || []) {
    panel.append(el("h3", `What ${d.interpreter} answered`));
    if (d.result) panel.append(Signal.answers(d.result));
    else panel.append(el("p", d.error || "No valid model result."));
  }
  const raw = el("details");
  raw.append(el("summary", "Normalized Event"), el("pre", evidence.event_json));
  panel.append(raw);
  $("review-evidence").replaceChildren(panel);
  $("category-options").replaceChildren(
    ...(review.categories || []).map((c) => {
      const option = el("option");
      option.value = c;
      return option;
    }),
  );
  panel.append(
    el(
      "p",
      review.contract_supported
        ? review.categories?.length
          ? `Allowed categories: ${review.categories.join(", ")}.`
          : "Category accepts a free-text string."
        : "This interpretation contract does not support category/action labels. Mark this event uncertain or deferred.",
    ),
  );
  $("review-history").textContent = json({
    decisions: evidence.decisions,
    current_label: review.label,
    history: review.history,
  });
  $("review-state").value =
    review.state === "unreviewed"
      ? review.contract_supported
        ? "reviewed"
        : "uncertain"
      : review.state;
  $("review-origin").value = review.origin;
  $("category").value = "";
  $("requires-action").value = "";
  $("expected-outcome").value = "";
  $("review-form").hidden = false;
}
action("load-review", loadReview);
$("review-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (!state.review) return;
  try {
    await api("/manage/api/review", {
      id: state.review.id,
      expected: state.review.revision,
      state: $("review-state").value,
      origin: $("review-origin").value,
      category: $("category").value,
      requires_action:
        $("requires-action").value === ""
          ? null
          : $("requires-action").value === "true",
      outcome: $("expected-outcome").value,
    });
    notice("Review saved. No event was rerun.");
    await Promise.all([loadReview(), reviewQueue()]);
  } catch (err) {
    fail(err.message);
  }
});
function template(bootstrap) {
  return `server:\n  addr: ${bootstrap.Server.Addr}\ndb:\n  path: ${JSON.stringify(bootstrap.DB.Path)}\nmodels:\n  judge:\n    role: decision\n    provider: openai-compatible\n    base_url: http://127.0.0.1:8000/v1\n    model: replace-with-your-model\ninterpret:\n  model: judge\n  prompt: Classify this event for a human operator.\n  schema:\n    category:\n      type: string\n      enum: [routine, incident]\n    requires_action:\n      type: boolean\npolicy:\n  - if:\n      result.category: incident\n    then:\n      notify:\n        via: stdout\n      shadow: true\ndefault:\n  ignore: true\nsources: []\n`;
}
function renderConfigRevision() {
  const data = state.config;
  if (!data) return;
  const pending = data.draft.yaml && data.draft.yaml !== data.active;
  const idle = !state.dirty && !pending;
  $("config-state").textContent =
    `${state.dirty ? "Unsaved changes" : pending ? "Saved draft · Not applied" : data.setup ? "Not configured" : "Active · No pending changes"}${pending && data.draft.base !== data.revision ? " · Based on an older revision; reload and review before applying" : ""}`;
  $("config-state").title = `Active revision: ${data.revision}`;
  // Violet is reserved for work that is not running; nothing else uses it.
  $("config-lamp").dataset.state = idle ? "clear" : "draft";
  $(
    "config-state",
  ).parentElement.parentElement.classList.toggle("is-draft", !idle);
  // With nothing to save on the monitor list, Save and Apply are noise. Forms
  // always keep them: the guide can be saved before any field has been changed.
  $("config-buttons").hidden = idle && state.configView === "current";
  $("save-draft").classList.toggle("primary", !!state.dirty);
  $("review-config").classList.toggle("primary", !state.dirty && !!pending);
}
function renderConnections() {
  $("connections").replaceChildren();
  for (const connection of (state.config.connections || []).sort((a, b) =>
    a.name.localeCompare(b.name),
  )) {
    $("connections").append(
      record(
        connection.name,
        `${connection.kind} · credential ${connection.credential}`,
      ),
    );
  }
}
async function loadConfig() {
  const data = await api("/manage/api/state");
  state.config = data;
  renderConnections();
  $("yaml").value = data.draft.yaml || data.active || template(data.bootstrap);
  state.dirty = false;
  $("apply-confirm").checked = false;
  $("setup-note").hidden = !data.setup;
  renderConfigRevision();
  $("revisions").replaceChildren();
  for (const h of data.history || []) {
    const n = record(
      h.id.slice(0, 12),
      `${stamp(h.at)} · ${h.id === data.revision ? "running now" : h.status === "active" ? "previously active" : h.status}`,
    );
    const b = el("button", "Restore into editor");
    b.addEventListener("click", async () => {
      if (state.dirty && !confirm("Replace unsaved editor changes?")) return;
      try {
        const prior = await api(
          `/manage/api/revision?id=${encodeURIComponent(h.id)}`,
        );
        $("yaml").value = prior.yaml;
        changed();
        await updateForms();
        notice(
          "Revision loaded into editor. It is not active. Validate and save before applying.",
        );
      } catch (err) {
        fail(err.message);
      }
    });
    n.append(b);
    $("revisions").append(n);
  }
  await updateForms();
  resetSetup();
  renderMonitors();
  configView("current");
}
function changed() {
  state.dirty = true;
  renderConfigRevision();
  invalidateSetupTest();
  $("apply-confirm").checked = false;
  $("validation").textContent =
    "Unsaved editor changes. Validate and save before activation.";
}
$("yaml").addEventListener("input", changed);
window.addEventListener("beforeunload", (e) => {
  if (state.dirty || $("setup-webhook-url").value) {
    e.preventDefault();
    e.returnValue = "";
  }
});
action("reload-config", async () => {
  if (!state.dirty || confirm("Discard unsaved editor changes?"))
    await loadConfig();
});
async function validated() {
  const original = $("yaml").value;
  const result = await api("/manage/api/validate", {
    yaml: original,
    expected: "",
    base: "",
    confirm: false,
  });
  if ($("yaml").value !== original)
    throw Error(
      "The editor changed during validation. Validate the current text again.",
    );
  $("validation").textContent = result.notice;
  return result.document;
}
action("validate", async () => {
  await validated();
  await updateForms();
});
async function saveDraft() {
  const text = $("yaml").value;
  const d = await api("/manage/api/draft", {
    yaml: text,
    expected: state.config.draft.revision || "",
    base: state.config.revision,
    confirm: false,
  });
  state.config.draft = d;
  state.dirty = $("yaml").value !== text;
  renderConfigRevision();
  notice("Draft saved. Running configuration is unchanged.");
  $("validation").textContent = `Saved draft ${d.revision.slice(0, 12)}.`;
}
action("save-draft", async () => {
  const guide = !$("setup-guide").hidden;
  if (guide && setupState.dirty)
    for (let step = 0; step < 3; step++) if (!validSetupStep(step)) return;
  $("config-state").textContent = "Saving configuration…";
  try {
    await configurationTask(async () => {
      if (
        guide &&
        $("setup-notify").value === "discord" &&
        $("setup-webhook-url").value
      )
        await saveDiscordConnection();
      if (guide && setupState.dirty) await buildSetup();
      await saveDraft();
    });
  } catch (error) {
    renderConfigRevision();
    $("config-state").textContent +=
      " · Save not confirmed; reload saved state before retrying.";
    throw error;
  }
});
action("review-config", async () => {
  if (!$("setup-guide").hidden) {
    await reviewSetup();
    return;
  }
  configView("advanced");
  diff();
  $("apply-confirm").focus();
});
function diff() {
  const old = (state.config.active || "").split("\n"),
    candidate = $("yaml").value.split("\n");
  const lines = [];
  for (let i = 0; i < Math.max(old.length, candidate.length); i++) {
    if (old[i] === candidate[i]) continue;
    if (old[i] !== undefined) lines.push(`- ${i + 1}: ${old[i]}`);
    if (candidate[i] !== undefined) lines.push(`+ ${i + 1}: ${candidate[i]}`);
  }
  $("diff-content").textContent = lines.join("\n") || "No differences.";
  $("diff").open = true;
}
action("show-diff", diff);
async function activateDraft() {
  if (state.dirty)
    throw Error("Save the exact editor draft before activation.");
  if (!$("apply-confirm").checked)
    throw Error(
      "Review the difference and confirm collection scope before activation.",
    );
  notice(
    "Checking source readiness and draining current polls. Keep this page open.",
  );
  await configurationTask(async () => {
    await api("/manage/api/activate", {
      yaml: $("yaml").value,
      expected: state.config.draft.revision,
      base: state.config.revision,
      confirm: true,
      allow_live_notify: true,
    });
    await loadConfig();
    await overview();
    notice("Saved configuration is active. New work uses this revision.");
  });
}
action("activate", activateDraft);

// Keep form values stable across requests. Restore disabled states rather than
// enabling controls whose readiness conditions have not been met.
async function configurationTask(fn) {
  state.configBusy = (state.configBusy || 0) + 1;
  const controls = [
    ...document.querySelectorAll(
      '[data-page="configure"] input,[data-page="configure"] select,[data-page="configure"] textarea,[data-page="configure"] button',
    ),
  ].map((c) => [c, c.disabled]);
  controls.forEach(([c]) => {
    c.disabled = true;
  });
  $("setup-form").setAttribute("aria-busy", "true");
  try {
    return await fn();
  } finally {
    controls.forEach(([c, disabled]) => {
      c.disabled = disabled;
    });
    state.configBusy--;
    if (!state.configBusy) {
      $("setup-form").removeAttribute("aria-busy");
      if (setupState.base) notificationFields();
    }
    refreshSetupStart();
  }
}
async function setDocument(doc) {
  const original = $("yaml").value;
  const result = await api("/manage/api/compose", { document: doc });
  if ($("yaml").value !== original)
    throw Error(
      "The editor changed while this form was processed. Retry the form without losing your edit.",
    );
  $("yaml").value = result.yaml;
  changed();
  await updateForms();
}
async function updateForms() {
  let doc;
  try {
    doc = await validated();
  } catch {
    return;
  }
  $("configured-models").replaceChildren();
  $("interpreter-model").replaceChildren();
  for (const [name, model] of Object.entries(doc.models || {})) {
    const n = record(
      name,
      `${model.role || "decision"} · ${model.provider} · ${model.model}`,
    );
    const edit = el("button", "Edit model form");
    edit.addEventListener("click", () => {
      for (const [id, value] of [
        ["model-name", name],
        ["model-role", model.role || "decision"],
        ["model-provider", model.provider],
        ["model-id", model.model],
        ["model-url", model.base_url],
        ["model-env", model.api_key_env],
      ])
        $(id).value = value || "";
      $("model-form").closest("details").open = true;
      $("model-name").focus();
    });
    n.append(edit);
    $("configured-models").append(n);
    if (model.role !== "agent") {
      const option = el("option", name);
      option.value = name;
      $("interpreter-model").append(option);
    }
  }
  $("interpreter-model").value = doc.interpret?.model || "";
  $("configured-rules").replaceChildren();
  for (const [index, rule] of (doc.policy || []).entries()) {
    const n = record(
      `Rule ${index + 1}`,
      `${json(rule.if)} → ${json(rule.then)}`,
    );
    for (const [label, shift] of [
      ["Move earlier", -1],
      ["Move later", 1],
      ["Remove rule", 0],
    ]) {
      const button = el("button", label);
      button.disabled =
        shift !== 0 &&
        (index + shift < 0 || index + shift >= doc.policy.length);
      button.addEventListener("click", async () => {
        try {
          const current = await validated();
          if (json(current.policy?.[index]) !== json(rule))
            throw Error(
              "Rules changed in the editor. Validate again before reordering.",
            );
          if (shift === 0) current.policy.splice(index, 1);
          else
            [current.policy[index], current.policy[index + shift]] = [
              current.policy[index + shift],
              current.policy[index],
            ];
          await setDocument(current);
        } catch (error) {
          fail(error.message);
        }
      });
      n.append(button, document.createTextNode(" "));
    }
    $("configured-rules").append(n);
  }
  $("configured-rules").append(el("p", `Default: ${json(doc.default || {})}`));
  $("configured-sources").replaceChildren();
  for (const source of doc.sources || []) {
    const n = record(
      source.name,
      `${source.type} · ${source.paused ? "paused" : "collecting when activated"} · ${source.event_type}`,
    );
    const edit = el("button", "Edit source form");
    edit.addEventListener("click", () => {
      for (const [id, value] of [
        ["source-name", source.name],
        ["source-type", source.type],
        ["source-event", source.event_type],
        ["source-every", source.every],
        ["source-units", (source.units || []).join(", ")],
        ["source-priority", source.priority || "warning"],
        ["source-scope", source.scope || "system"],
        ["source-url", source.url],
        ["source-items", source.item_path],
        ["source-cursor", source.cursor_field],
        ["source-env", source.bearer_token_env],
      ])
        $(id).value = value || "";
      sourceFields();
      $("source-form").closest("details").open = true;
      $("source-name").focus();
    });
    n.append(edit, document.createTextNode(" "));
    const toggle = el(
      "button",
      source.paused ? "Resume in draft" : "Pause in draft",
    );
    toggle.addEventListener("click", async () => {
      try {
        const current = await validated();
        const target = current.sources.find((s) => s.name === source.name);
        if (!target) throw Error("Source was removed from editor.");
        target.paused = !target.paused;
        await setDocument(current);
      } catch (e) {
        fail(e.message);
      }
    });
    const remove = el("button", "Remove from draft");
    remove.addEventListener("click", async () => {
      if (
        !confirm(
          `Remove ${source.name} from the draft? Its cursor remains stored.`,
        )
      )
        return;
      try {
        const current = await validated();
        current.sources = current.sources.filter((s) => s.name !== source.name);
        // Removing a source also removes its routing contract, never its cursor.
        if (current.monitors) delete current.monitors[source.name];
        await setDocument(current);
      } catch (e) {
        fail(e.message);
      }
    });
    n.append(toggle, document.createTextNode(" "), remove);
    $("configured-sources").append(n);
  }
  return doc;
}

// Hide transport-specific fields; an irrelevant field must not look required.
function sourceFields() {
  const journal = $("source-type").value === "systemd-journal";
  for (const id of ["source-units", "source-priority", "source-scope"])
    $(id).parentElement.hidden = !journal;
  for (const id of [
    "source-url",
    "source-items",
    "source-cursor",
    "source-env",
  ])
    $(id).parentElement.hidden = journal;
}
$("source-type").addEventListener("change", sourceFields);
sourceFields();
$("model-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const doc = await validated();
    doc.models ||= {};
    const model = {
      role: $("model-role").value,
      provider: $("model-provider").value,
      model: $("model-id").value,
      base_url: $("model-url").value,
      ...($("model-env").value ? { api_key_env: $("model-env").value } : {}),
    };
    doc.models = { ...doc.models, [$("model-name").value]: model };
    await setDocument(doc);
  } catch (err) {
    fail(err.message);
  }
});
$("source-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const doc = await validated(),
      type = $("source-type").value;
    const prior =
      (doc.sources || []).find(
        (s) => s.name === $("source-name").value && s.type === type,
      ) || {};
    const source = {
      ...prior,
      name: $("source-name").value,
      type,
      event_type: $("source-event").value,
      every: $("source-every").value,
    };
    if (type === "systemd-journal") {
      source.units = $("source-units")
        .value.split(",")
        .map((s) => s.trim())
        .filter(Boolean);
      source.priority = $("source-priority").value;
      source.scope = $("source-scope").value;
    } else {
      source.url = $("source-url").value;
      for (const [field, id] of [
        ["item_path", "source-items"],
        ["cursor_field", "source-cursor"],
        ["bearer_token_env", "source-env"],
      ])
        if ($(id).value) source[field] = $(id).value;
        else delete source[field];
    }
    doc.sources = (doc.sources || []).filter((s) => s.name !== source.name);
    doc.sources.push(source);
    await setDocument(doc);
  } catch (err) {
    fail(err.message);
  }
});
$("interpreter-model").addEventListener("change", async () => {
  try {
    const name = $("interpreter-model").value;
    const doc = await validated();
    doc.interpret.model = name;
    await setDocument(doc);
  } catch (error) {
    fail(error.message);
  }
});
action("load-active", async () => {
  if (
    state.dirty &&
    !confirm("Replace unsaved editor changes with the active configuration?")
  )
    return;
  const data = await api("/manage/api/state");
  state.config = data;
  $("yaml").value = data.active || template(data.bootstrap);
  changed();
  await updateForms();
});
action("shadow-draft", async () => {
  const doc = await validated();
  for (const contract of [doc, ...Object.values(doc.monitors || {})]) {
    for (const rule of contract.policy || [])
      if (!rule.then.ignore) rule.then.shadow = true;
    if (contract.default && !contract.default.ignore)
      contract.default.shadow = true;
  }
  await setDocument(doc);
});
$("policy-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const doc = await validated();
    let value = $("rule-value").value;
    try {
      value = JSON.parse(value);
    } catch {}
    const then =
      $("rule-outcome").value === "ignore"
        ? { ignore: true }
        : {
            notify: {
              via: $("rule-adapter").value,
              ...($("rule-adapter").value === "discord"
                ? { webhook_env: $("rule-env").value }
                : {}),
            },
            shadow: true,
          };
    doc.policy ||= [];
    doc.policy.push({ if: { [$("rule-field").value]: value }, then });
    await setDocument(doc);
  } catch (error) {
    fail(error.message);
  }
});
action("shadow", async () => {
  if (
    !confirm(
      "Block all effects, including in-flight work not yet dispatched? This durable gate cannot be lifted from this UI.",
    )
  )
    return;
  await api("/manage/api/shadow", { confirm: true });
  state.config.shadow_override = true;
  renderMonitors();
  await overview();
  notice("Effects blocked. Already-dispatched effects cannot be recalled.");
});
async function audit() {
  const rows = await api("/manage/api/audit");
  $("audit").replaceChildren();
  if (!rows.length) {
    const none = el("p", "No operator changes recorded yet.");
    none.className = "panel help";
    $("audit").append(none);
    return;
  }
  const { wrap, body } = table(
    ["When", "Operation", "Result", "Actor", "Target"],
    "audit-table",
  );
  for (const row of rows) {
    const line = el("tr");
    line.append(
      cell(stamp(row.at), "cell-time"),
      cell(row.operation, "cell-subject"),
      cell(row.result),
      cell(row.actor),
      cell(row.target),
    );
    body.append(line);
  }
  $("audit").append(wrap);
}
action("refresh-audit", audit);
async function evaluations() {
  const rows = await api("/manage/api/evaluations");
  $("evaluations").replaceChildren();
  for (const row of rows) {
    const d = el("details");
    d.append(
      el("summary", `${stamp(row.at)} · ${row.status} · ${row.id}`),
      el("pre", json(row.report)),
    );
    $("evaluations").append(d);
  }
  if (!rows.length) $("evaluations").append(el("p", "No saved evaluations."));
}
action("refresh-evals", evaluations);
action("evaluate", async () => {
  if (!$("eval-confirm").checked)
    throw Error("Confirm evaluation scope and data destination first.");
  const result = await api("/manage/api/evaluate", {
    yaml: $("yaml").value,
    mode: $("eval-mode").value,
    confirm: true,
  });
  notice(
    `Evaluation ${result.id} started for ${result.total} events. Refresh reports to inspect progress.`,
  );
  await evaluations();
});
action("cancel-eval", async () => {
  await api("/manage/api/cancel-evaluation", {});
  await evaluations();
  notice("Cancellation requested. Completed rows remain in the report.");
});
