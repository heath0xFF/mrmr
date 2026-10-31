"use strict";

// Current configuration is always projected from the active document. The YAML
// editor may contain a newer draft; showing that as running would hide failures.
function monitorContract(doc, source) {
  if (Object.hasOwn(doc.monitors || {}, source.name))
    return doc.monitors[source.name];
  return {
    interpret: Object.hasOwn(doc.interpret_by_type || {}, source.event_type)
      ? doc.interpret_by_type[source.event_type]
      : doc.interpret,
    policy: doc.policy || [],
    default: doc.default || { ignore: true },
  };
}
function configView(view) {
  $("current-config").hidden = view !== "current";
  $("setup-guide").hidden = view !== "guide";
  $("advanced-config").hidden = view !== "advanced";
  $("back-current").hidden = view === "current";
  $("config-title").textContent =
    view === "current"
      ? "Current configuration"
      : view === "advanced"
        ? "Edit configuration draft"
        : setupState.sourceName
          ? "Edit monitoring service"
          : "Add monitoring service";
  $("config-description").textContent =
    view === "current"
      ? "The monitoring services in the active revision. Draft changes are not running."
      : "Save monitor edits as a draft, then Apply to run them with the selected notification mode. Connection saves take effect immediately.";
}
function renderMonitors() {
  const doc = state.config.document;
  $("active-monitors").replaceChildren();
  if (!doc?.sources?.length) {
    $("active-monitors").append(
      el("h2", "No monitoring services yet"),
      el(
        "p",
        "Add a service to select its model, collection scope, and notification destination.",
      ),
    );
    return;
  }
  for (const source of doc.sources) {
    const contract = monitorContract(doc, source);
    const effects = [
      ...(contract.policy || []).map((r) => r.then),
      contract.default || {},
    ].filter((t) => t.notify || t.action || t.delegate);
    const live = effects.some((t) => !t.shadow);
    const mode = live
      ? state.config.shadow_override
        ? "Live rules blocked by safety gate"
        : "Live effects configured"
      : effects.length
        ? "Shadow · no messages sent"
        : "Ignore only";
    const article = el("article"),
      heading = el("div");
    article.className = "monitor-record";
    heading.className = "heading";
    heading.append(
      el("h2", source.name),
      el("p", `${source.paused ? "Paused" : "Enabled"} · ${mode}`),
    );
    article.append(heading);
    const facts = el("dl");
    facts.className = "monitor-facts";
    const destinations = [
      ...new Set(
        effects.map((t) =>
          t.notify
            ? t.notify.via === "discord"
              ? `Discord · ${t.notify.webhook_env}`
              : "Runtime log (stdout)"
            : t.action
              ? "Action (Advanced)"
              : "Delegation (Advanced)",
        ),
      ),
    ];
    const model = doc.models?.[contract.interpret?.model];
    for (const [label, value] of [
      [
        "Collection",
        source.type === "systemd-journal"
          ? `${source.scope || "system"} units · ${(source.units || []).join(", ")} · ${source.priority} and higher · every ${source.every}`
          : `${source.type} · ${source.event_type} · every ${source.every}`,
      ],
      [
        "Model",
        `${contract.interpret?.model || "Unknown"} · ${model?.provider || "Unknown provider"}`,
      ],
      ["Notifications / effects", destinations.join("; ") || "None"],
    ]) {
      const group = el("div");
      group.append(el("dt", label), el("dd", value));
      facts.append(group);
    }
    article.append(facts);
    const ownership = Object.hasOwn(doc.monitors || {}, source.name)
      ? "Independent monitor rules."
      : source.type === "systemd-journal"
        ? "Uses shared legacy rules. Edit monitor creates independent rules for this source."
        : "Uses shared legacy rules. Source-local contracts can be set in Advanced YAML.";
    article.append(el("p", ownership));
    for (const ref of new Set(
      effects
        .filter((t) => t.notify?.via === "discord")
        .map((t) => t.notify.webhook_env),
    )) {
      const connection = state.config.connections?.find(
        (c) => c.kind === "Discord" && c.name === ref,
      );
      const status = el(
        "p",
        `Discord credential ${ref}: ${connection?.credential || "unknown"}. Delivery not verified.`,
      );
      status.className = "help";
      article.append(status);
    }
    const buttons = el("div");
    buttons.className = "row";
    for (const [label, fn] of [
      [
        source.type === "systemd-journal" ? "Edit monitor" : "Edit in Advanced",
        async () => {
          if (source.type === "systemd-journal")
            await beginMonitor(source.name);
          else await openAdvanced();
        },
      ],
      [
        source.paused ? "Resume collection" : "Pause collection",
        async () => {
          if (
            !confirm(
              `${source.paused ? "Resume" : "Pause"} collection for ${source.name}? Its saved position is retained. This does not stop the service itself.`,
            )
          )
            return;
          if (!(await useActiveDocument())) return;
          const doc = await validated();
          const target = doc.sources.find((s) => s.name === source.name);
          if (!target || !!target.paused !== !!source.paused)
            throw Error(
              "Source state changed. Reload current configuration before retrying.",
            );
          target.paused = !target.paused;
          await configurationTask(async () => {
            await setDocument(doc);
            await saveDraft();
            $("apply-confirm").checked = true;
            await activateDraft();
          });
        },
      ],
    ]) {
      const button = el("button", label);
      button.addEventListener("click", async () => {
        button.disabled = true;
        try {
          await fn();
        } catch (error) {
          notice(error.message);
        } finally {
          button.disabled = false;
        }
      });
      buttons.append(button);
    }
    article.append(buttons);
    $("active-monitors").append(article);
  }
  if (doc.filter?.length)
    $("active-monitors").append(
      el(
        "p",
        `${doc.filter.length} shared pre-model filters apply to all monitors. See Advanced configuration.`,
      ),
    );
}

// Edits from the current-config list start from active state, not an unrelated
// saved draft. Replacing either pending form work or a saved draft needs consent.
async function useActiveDocument() {
  const pending =
    state.dirty ||
    (state.config.draft.yaml &&
      state.config.draft.yaml !== state.config.active);
  if (
    pending &&
    !confirm(
      "Start from the active configuration instead of the pending draft? The saved draft is replaced only when you save this edit.",
    )
  )
    return false;
  $("yaml").value = state.config.active || template(state.config.bootstrap);
  state.dirty = false;
  invalidateSetupTest();
  return true;
}
async function beginMonitor(name = null) {
  if (!(await useActiveDocument())) return;
  loadSetup(await updateForms(), name);
  setupStep(0);
}
async function openAdvanced() {
  if (!$("setup-guide").hidden && setupState.dirty)
    await configurationTask(buildSetup);
  invalidateSetupTest();
  configView("advanced");
  $("open-guided").focus();
}
action("add-monitor", () => beginMonitor());
action("back-current", async () => {
  if (setupState.dirty) {
    if (
      !confirm(
        "Discard unreviewed form changes and return to current configuration?",
      )
    )
      return;
    setupState.dirty = false;
    state.dirty =
      $("yaml").value !==
      (state.config.draft.yaml ||
        state.config.active ||
        template(state.config.bootstrap));
    renderConfigRevision();
  }
  invalidateSetupTest();
  configView("current");
  renderMonitors();
  $("add-monitor").focus();
});
action("open-advanced", openAdvanced);
action("setup-open-advanced", openAdvanced);
action("open-guided", async () => {
  loadSetup(await validated());
  setupStep(0);
});
