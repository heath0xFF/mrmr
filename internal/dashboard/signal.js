"use strict";

// Signal is the status vocabulary shared by /dashboard/ and /manage/. It lives in
// one file because the two pages must never disagree about whether a stage is
// stopped: the read-only page and the control page have separate authority, but
// they read the same evidence and should state it the same way.
//
// This code only describes stored evidence. A green lamp means "the latest record
// for this stage succeeded", never "this service is healthy right now", so every
// lamp carries text and configuration-only stages use the neutral "set" state.
//
// Everything rendered here comes from event data, configuration names, or model
// output. It is untrusted, so nodes are built with textContent and never as HTML.
const Signal = (() => {
  const node = (tag, text, className) => {
    const n = document.createElement(tag);
    if (text !== undefined) n.textContent = text;
    if (className) n.className = className;
    return n;
  };
  // Go's zero time arrives as year 0001. Treating it as a date would report
  // sources that never polled as having polled two thousand years ago.
  const date = (value) =>
    value && !String(value).startsWith("0001-") ? new Date(value) : null;
  function age(value, now = new Date()) {
    const at = date(value);
    if (!at) return "never";
    const seconds = Math.max(0, Math.floor((now - at) / 1000));
    if (seconds < 60) return `${seconds}s ago`;
    if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
    if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
    return `${Math.floor(seconds / 86400)}d ago`;
  }
  // Today's rows only need a clock. Older rows need the date, or two rows with
  // the same time from different days would read as simultaneous.
  function clock(value) {
    const at = date(value);
    if (!at) return "—";
    const today = at.toDateString() === new Date().toDateString();
    return today
      ? at.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })
      : at.toLocaleDateString([], { month: "short", day: "numeric" }) +
          " " +
          at.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  }
  const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;

  // A poll older than three intervals is overdue. The 30s floor stops short
  // intervals from flickering to caution on ordinary scheduling jitter.
  function collectStage(source, data) {
    const h = source.health,
      now = new Date(data.at),
      started = date(h.started_at),
      finished = date(h.finished_at),
      limit = Math.max(30000, source.every_ms * 3);
    if (source.paused)
      return { state: "off", detail: "Paused, cursor retained" };
    if (h.running)
      return started && now - started > limit
        ? { state: "caution", detail: `Poll running since ${age(started, now)}` }
        : { state: "clear", detail: "Polling now" };
    if (h.failed)
      return { state: "stop", detail: `Failed ${age(finished, now)}` };
    if (finished)
      return now - finished > limit
        ? { state: "caution", detail: `No poll since ${age(finished, now)}` }
        : { state: "clear", detail: age(finished, now) };
    // Runtime age, not poll age: a source that has never run is only suspicious
    // once the runtime has been up long enough that it should have.
    return now - new Date(data.started_at) > limit
      ? { state: "caution", detail: "First poll not seen" }
      : { state: "off", detail: "Waiting for first poll" };
  }

  // Precedence mirrors the runtime: a source-local contract wins, then an event
  // type override, then the shared default interpreter.
  function modelFor(source, data) {
    const local = data.monitor_interpreters || {},
      types = data.interpret_overrides || {};
    if (Object.hasOwn(local, source.name)) return local[source.name];
    if (Object.hasOwn(types, source.event_type)) return types[source.event_type];
    return data.interpreter || "";
  }
  // Timings are per interpreter across all sources, because the runtime does not
  // record them per source. The text therefore names the model, not the track.
  function judgeStage(name, data) {
    const rows = (data.summary.models || []).filter(
      (m) => m.interpreter === name,
    );
    const label = name || "No model";
    if (!rows.length)
      return { state: "off", detail: `${label}, no decisions yet` };
    const latest = rows.reduce((a, b) =>
      (date(a.last_at) || 0) > (date(b.last_at) || 0) ? a : b,
    );
    const errors = rows.reduce((n, m) => n + m.errors, 0);
    if (latest.last_status !== "ok")
      return { state: "stop", detail: `${label}, last decision failed` };
    if (errors)
      return { state: "caution", detail: `${label}, ${plural(errors, "error")}` };
    return {
      state: "clear",
      detail:
        latest.median_ms == null ? label : `${label} · ${latest.median_ms} ms`,
    };
  }
  // Without its own contract a source runs the shared rules, so "rule:" names
  // apply and every "monitor:" name belongs to some other source.
  function rulesFor(source, data) {
    const local = Object.hasOwn(data.monitor_interpreters || {}, source.name);
    const prefix = `monitor:${source.name}/`;
    return (data.policy || []).filter((r) =>
      local ? r.rule.startsWith(prefix) : !r.rule.startsWith("monitor:"),
    );
  }
  function decideStage(rules) {
    const count = rules.filter((r) => !r.rule.endsWith("default")).length;
    // Configured, not observed. A green lamp here would present YAML as evidence.
    return { state: "set", detail: `${plural(count, "rule")} + default` };
  }
  function effectStage(rules, data) {
    const effects = rules.filter((r) => r.outcome !== "ignore"),
      live = effects.filter((r) => !r.shadow).length;
    if (data.effects_blocked)
      return { state: "stop", detail: "Blocked by safety gate" };
    if (live)
      return { state: "clear", detail: `Live, ${plural(live, "rule")}` };
    if (effects.length) return { state: "hold", detail: "Shadow, nothing sent" };
    return { state: "off", detail: "Ignore only" };
  }
  function track(source, data) {
    const collect = collectStage(source, data),
      model = modelFor(source, data),
      rules = rulesFor(source, data);
    // Downstream lamps go dark when nothing can reach them, but keep their text.
    // Model timings are shared across sources, so a green lamp after a stopped
    // Collect would imply this track's work is getting through when it is not.
    const flowing = collect.state !== "stop" && !source.paused;
    const judge = judgeStage(model, data);
    return {
      name: source.name,
      model,
      paused: !!source.paused,
      stages: [
        { label: "Collect", ...collect },
        { label: "Judge", ...judge, state: flowing ? judge.state : "off" },
        { label: "Decide", ...decideStage(rules) },
        { label: "Effect", ...effectStage(rules, data) },
      ],
    };
  }

  // The headline answers "is all well?". Shadow and paused are deliberate operator
  // choices, so neither raises it; only stops and cautions do.
  function assess(data) {
    const tracks = (data.sources || []).map((s) => track(s, data));
    const worst = (state) =>
      tracks.flatMap((t) =>
        t.stages
          .filter((s) => s.state === state && s.label !== "Effect")
          .map((s) => ({ track: t, stage: s })),
      );
    const stops = worst("stop"),
      cautions = worst("caution"),
      active = tracks.filter((t) => !t.paused),
      pending = data.summary.pending || 0;
    const clear = active.filter((t) =>
      t.stages.every((s) => !["stop", "caution"].includes(s.state)),
    ).length;
    const rules = data.policy || [],
      effects = rules.filter((r) => r.outcome !== "ignore"),
      live = effects.filter((r) => !r.shadow).length;
    const effect = data.effects_blocked
      ? "The safety gate is blocking all effects."
      : live
        ? `${plural(live, "live rule")} can send notifications.`
        : effects.length
          ? "All effects are held in shadow."
          : "Policy selects ignore only.";
    let title, tone;
    if (data.effects_blocked && !stops.length && !cautions.length) {
      title = "Effects are stopped by the safety gate";
      tone = "stop";
    } else if (stops.length === 1) {
      title = `${stops[0].track.name} is stopped at ${stops[0].stage.label}`;
      tone = "stop";
    } else if (stops.length) {
      title = `${stops.length} stages are stopped`;
      tone = "stop";
    } else if (cautions.length === 1) {
      title = `${cautions[0].track.name} needs a look at ${cautions[0].stage.label}`;
      tone = "caution";
    } else if (cautions.length) {
      title = `${cautions.length} stages need a look`;
      tone = "caution";
    } else if (pending) {
      title = `${plural(pending, "effect")} unfinished`;
      tone = "caution";
    } else if (!tracks.length) {
      title = "No monitors yet";
      tone = "off";
    } else if (!active.length) {
      title = "All tracks are paused";
      tone = "off";
    } else {
      title = "All tracks clear";
      tone = "clear";
    }
    const count = tracks.length
      ? `${clear} of ${plural(active.length, "active track")} clear. `
      : "HTTP ingress remains available. ";
    return { title, tone, sentence: count + effect, tracks };
  }

  function lamp(state, label) {
    const n = node("span", undefined, "lamp");
    n.dataset.state = state;
    if (label) n.title = label;
    return n;
  }
  // Same markup for monitors, event histories, and guide progress: learning to
  // read one track teaches the others, which is the point of the signature move.
  function stageList(stages) {
    const list = node("ol", undefined, "track-line");
    for (const s of stages) {
      const item = node("li", undefined, "stage");
      item.dataset.state = s.state;
      item.append(
        lamp(s.state),
        node("strong", s.label),
        node("span", s.detail, "stage-detail"),
      );
      list.append(item);
    }
    return list;
  }
  // Refreshing replaces nodes every ten seconds. Without this, a keyboard user's
  // focus would silently fall back to the document on each refresh.
  function keepFocus(render) {
    const key = document.activeElement?.dataset?.keepId;
    render();
    if (key)
      for (const n of document.querySelectorAll("[data-keep-id]"))
        if (n.dataset.keepId === key) {
          n.focus({ preventScroll: true });
          break;
        }
  }
  function renderTracks(root, tracks, open) {
    keepFocus(() => {
      root.replaceChildren();
      for (const t of tracks) {
        const row = node("article", undefined, "track");
        const name = node("a", t.name, "track-name");
        name.href = open.href(t.name);
        name.dataset.keepId = `track:${t.name}`;
        name.title = `Show events from ${t.name}`;
        name.addEventListener("click", (e) => {
          if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey)
            return;
          e.preventDefault();
          open.go(t.name);
        });
        row.append(name, stageList(t.stages));
        root.append(row);
      }
    });
  }

  // Applies to the Overview's attention list and to every outcome column.
  function outcome(e) {
    if (["errored", "invalid"].includes(e.decision_status))
      return { state: "stop", text: `Model ${e.decision_status}` };
    if (!e.outcome) return { state: "off", text: "No execution yet" };
    if (e.execution_status === "error")
      return { state: "stop", text: `${e.outcome} failed` };
    if (e.execution_status === "pending")
      return { state: "caution", text: `${e.outcome} unfinished` };
    if (e.execution_status === "shadow")
      return { state: "hold", text: `${e.outcome} · shadow` };
    if (e.outcome === "ignore")
      return {
        state: "off",
        text: e.execution_status === "filtered" ? "Filtered" : "Ignored",
      };
    return { state: "clear", text: e.outcome };
  }
  function outcomeTag(e) {
    const o = outcome(e),
      tag = node("span", undefined, "outcome");
    tag.dataset.state = o.state;
    tag.append(lamp(o.state), document.createTextNode(o.text));
    return tag;
  }
  // Worth an operator's attention: anything the policy did not ignore, and any
  // model failure, because failures route to ignore and would otherwise be hidden.
  const needsAttention = (e) => outcome(e).state !== "off";

  // Typed answers arrive as {choice, confidence} or {noul} objects and legacy
  // answers as bare values. Show the verdict first, then any supporting numbers.
  function answerText(value) {
    if (value === null || typeof value !== "object") return String(value);
    const primary = ["choice", "value", "score"].find((k) =>
      Object.hasOwn(value, k),
    );
    const rest = Object.entries(value)
      .filter(([k, v]) => k !== primary && typeof v !== "object")
      .map(([k, v]) => `${k} ${v}`);
    return [primary ? String(value[primary]) : "", ...rest]
      .filter(Boolean)
      .join(" · ");
  }
  function answers(result) {
    const list = node("dl", undefined, "answers");
    for (const [key, value] of Object.entries(result || {}))
      list.append(node("dt", key.replaceAll("_", " ")), node("dd", answerText(value)));
    return list;
  }

  // One event's path through the same four stages a monitor's track shows.
  function eventStages(ins) {
    const d = (ins.decisions || []).at(-1),
      x = (ins.executions || []).at(-1);
    const filtered = x?.status === "filtered";
    const judge = filtered
      ? { state: "off", detail: "Filtered before the model" }
      : !d
        ? { state: "off", detail: "No decision recorded" }
        : d.status === "ok"
          ? {
              state: "clear",
              detail: `${d.interpreter}${d.latency_ms == null ? "" : ` · ${d.latency_ms} ms`}`,
            }
          : { state: "stop", detail: `${d.interpreter}, ${d.status}` };
    const decide = x
      ? { state: "set", detail: `${x.policy_rule || "Policy"} → ${x.outcome}` }
      : { state: "off", detail: "No outcome recorded" };
    const effect = !x
      ? { state: "off", detail: "No execution recorded" }
      : x.status === "shadow"
        ? { state: "hold", detail: "Shadow, no effect performed" }
        : x.status === "pending"
          ? { state: "caution", detail: "Unfinished, may or may not have occurred" }
          : x.status === "error"
            ? { state: "stop", detail: `${x.adapter || x.outcome} failed` }
            : x.outcome === "ignore"
              ? { state: "off", detail: "No effect" }
              : { state: "clear", detail: `${x.adapter || x.outcome} reported success` };
    return [
      {
        label: "Collect",
        state: "clear",
        detail: `From ${ins.event.source}`,
      },
      { label: "Judge", ...judge },
      { label: "Decide", ...decide },
      { label: "Effect", ...effect },
    ];
  }

  return {
    node,
    date,
    age,
    clock,
    plural,
    assess,
    lamp,
    stageList,
    keepFocus,
    renderTracks,
    outcome,
    outcomeTag,
    needsAttention,
    answers,
    eventStages,
  };
})();
