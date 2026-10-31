"use strict";

// The guide changes one source-local contract. Shared connections, legacy rules,
// and other monitors remain byte-for-byte equivalent as decoded configuration.
const setupState = {
  base: null,
  sourceName: null,
  step: 0,
  test: null,
  candidate: "",
  dirty: false,
};
function resetSetup() {
  Object.assign(setupState, {
    base: null,
    sourceName: null,
    dirty: false,
    test: null,
    candidate: "",
  });
  $("setup-form").reset();
  for (const id of ["setup-summary", "setup-connection", "setup-webhook"])
    $(id).replaceChildren();
  for (const id of [
    "setup-status",
    "setup-cursor",
    "setup-connection-info",
    "setup-discord-status",
  ])
    $(id).textContent = "";
  invalidateSetupTest();
}
function loadSetup(doc, sourceName = null) {
  if (!doc) throw Error("Validate the draft before opening the monitor guide.");
  resetSetup();
  setupState.base = doc;
  setupState.sourceName = sourceName;
  const source = (doc.sources || []).find((s) => s.name === sourceName);
  if (sourceName && (!source || source.type !== "systemd-journal"))
    throw Error(
      "This guide edits system-journal sources only. Use Advanced configuration.",
    );
  const prior = source ? monitorContract(doc, source) : null;
  $("setup-name").value = source?.name || "";
  $("setup-units").value = (source?.units || []).join(", ");
  $("setup-priority").value = source?.priority || "warning";
  $("setup-every").value = source?.every || "30s";
  $("setup-cursor").textContent = source
    ? `Its name and saved journal position are retained.${source.paused ? " It remains paused. Resume collection from Current configuration when ready." : " Collection continues from its saved position."}`
    : "A new monitor starts at the current journal tail. Older warnings are not imported.";
  for (const [name, m] of Object.entries(doc.models || {})) {
    if (m.role === "agent" || m.model === "replace-with-your-model") continue;
    const option = el("option", `${name} · ${m.provider}`);
    option.value = name;
    $("setup-connection").append(option);
  }
  const newConnection = el("option", "Connect another model…");
  newConnection.value = "";
  $("setup-connection").append(newConnection);
  const modelName = prior?.interpret?.model || doc.interpret?.model;
  if ([...$("setup-connection").options].some((o) => o.value === modelName))
    $("setup-connection").value = modelName;
  $("setup-provider").value = "liquid-d1";
  $("setup-model").value = "";
  $("setup-url").value = "";
  $("setup-env").value = "";
  $("setup-connection-name").value = "";
  const first = prior?.policy?.[0];
  $("setup-notify").value =
    first?.then?.notify?.via === "stdout" ? "stdout" : "discord";
  const references = new Set(["MRMR_DISCORD_WEBHOOK_URL"]);
  for (const p of [doc, ...Object.values(doc.monitors || {})])
    for (const t of [...(p.policy || []).map((r) => r.then), p.default || {}])
      if (t.notify?.via === "discord") references.add(t.notify.webhook_env);
  for (const ref of references) {
    const option = el("option", ref);
    option.value = ref;
    $("setup-webhook").append(option);
  }
  const custom = el("option", "Use another environment variable…");
  custom.value = "";
  $("setup-webhook").append(custom);
  $("setup-webhook").value =
    first?.then?.notify?.webhook_env || "MRMR_DISCORD_WEBHOOK_URL";
  for (const [id, field] of [
    ["setup-confidence", "result.category.confidence"],
    ["setup-action-probability", "result.requires_action.noul"],
  ]) {
    const match = String(first?.if?.[field] || "").match(
      /^>=\s*(0(?:\.\d+)?|1(?:\.0+)?)$/,
    );
    $(id).value = match ? match[1] : "0.85";
  }
  $("setup-start").textContent = source
    ? "Apply monitor in shadow"
    : "Start in shadow";
  connectionFields();
  notificationFields();
  refreshSetupStart();
  configView("guide");
  setupStep(0, false);
}
function connectionFields() {
  const name = $("setup-connection").value;
  $("setup-new-connection").hidden = !!name;
  for (const input of $("setup-new-connection").querySelectorAll(
    "input,select",
  ))
    input.disabled = !!name;
  const model = setupState.base?.models?.[name];
  $("setup-connection-info").textContent = model
    ? `${model.model} at ${model.base_url}. ${model.api_key_env ? `Uses stored credential reference ${model.api_key_env}; no need to enter it again.` : "No credential reference."}`
    : "A new named connection can be reused by other monitors.";
  const required = ["liquid-d1", "typesafe"].includes(
    $("setup-provider").value,
  );
  $("setup-env").required = required;
  $("setup-credential-optional").hidden = required;
  notificationFields();
}
function selectedProvider() {
  return $("setup-connection").value
    ? setupState.base.models[$("setup-connection").value].provider
    : $("setup-provider").value;
}
function notificationFields() {
  const discord = $("setup-notify").value === "discord";
  const ref = $("setup-webhook").value;
  $("setup-discord").hidden = !discord;
  $("setup-webhook").disabled = !discord;
  $("setup-webhook-new").hidden = !!ref;
  $("setup-webhook-env").disabled = !discord || !!ref;
  const connection = state.config?.connections?.find(
    (c) => c.kind === "Discord" && c.name === ref,
  );
  $("setup-discord-status").textContent = ref
    ? `${ref}: ${connection?.credential || "not checked; provision this reference on the runtime host"}. Shadow mode does not send messages.`
    : "Enter the variable name only. Provision the webhook in the runtime service environment, then restart mrmr.";
  const generated = selectedProvider() === "openai-compatible";
  $("setup-thresholds").hidden = generated;
  for (const input of $("setup-thresholds").querySelectorAll("input"))
    input.disabled = generated;
  $("setup-rule-help").textContent = generated
    ? "Notify when category is incident and action is needed. Generated answers do not provide measured probabilities. Otherwise ignore."
    : "Notify when category is incident and both thresholds pass. Otherwise ignore. Thresholds are provisional until reviewed against real Events.";
}
function setupStep(step, focus = true) {
  setupState.step = step;
  $("setup-intro").hidden = step !== 0;
  for (const fieldset of document.querySelectorAll("[data-setup-step]"))
    fieldset.hidden = Number(fieldset.dataset.setupStep) !== step;
  for (const marker of document.querySelectorAll("[data-setup-marker]")) {
    if (Number(marker.dataset.setupMarker) === step)
      marker.setAttribute("aria-current", "step");
    else marker.removeAttribute("aria-current");
  }
  if (focus) {
    const legend = document.querySelector(`[data-setup-step="${step}"] legend`);
    legend.tabIndex = -1;
    legend.focus();
  }
}
function validSetupStep(step) {
  for (const input of document.querySelectorAll(
    `[data-setup-step="${step}"] input,[data-setup-step="${step}"] select`,
  ))
    if (!input.disabled && !input.reportValidity()) return false;
  return true;
}
function invalidateSetupTest() {
  setupState.test = null;
  $("setup-confirm").checked = false;
  $("setup-test-result").textContent =
    "Not tested. Saving a valid configuration does not prove that a model can answer.";
  refreshSetupStart();
}
function refreshSetupStart() {
  $("setup-start").disabled =
    !!state.configBusy ||
    !setupState.test?.ready ||
    setupState.test.yaml !== $("yaml").value ||
    !$("setup-confirm").checked;
  if (!state.configBusy) $("setup-name").disabled = !!setupState.sourceName;
}
$("setup-confirm").addEventListener("change", refreshSetupStart);
$("setup-form").addEventListener("input", (e) => {
  if (e.target.id === "setup-confirm") return;
  setupState.dirty = true;
  changed();
  $("setup-status").textContent =
    "Changes are not active. Review and test this draft before applying it.";
});
$("setup-connection").addEventListener("change", connectionFields);
$("setup-provider").addEventListener("change", connectionFields);
$("setup-notify").addEventListener("change", notificationFields);
$("setup-webhook").addEventListener("change", notificationFields);
$("setup-form").addEventListener("submit", (e) => {
  e.preventDefault();
  if (setupState.step < 3)
    $(
      ["setup-next-source", "setup-next-model", "setup-next-notify"][
        setupState.step
      ],
    ).click();
});

function serviceInterpretation(model) {
  return {
    model,
    questions: {
      category: {
        type: "choice",
        instructions:
          "Which category best describes this system service journal event?",
        criteria: {
          incident:
            "A service is broken, unavailable, repeatedly failing, or losing data now.",
          noise:
            "Routine startup, shutdown, retry, or warning chatter that needs no operator attention.",
          FYI: "Useful operational information that is neither an incident nor routine noise.",
        },
      },
      requires_action: {
        type: "noul",
        instructions:
          "Does this event indicate that the home-server operator genuinely needs to take action?",
        criteria: {
          true: "Intervention or investigation by the operator is warranted.",
          false: "No operator action is warranted from this event alone.",
        },
      },
      severity: {
        type: "score",
        instructions:
          "How severe is the operational impact described by this event?",
        criteria: [
          "No meaningful service impact.",
          "A service is degraded, but a workaround or automatic recovery exists.",
          "A service is unavailable or data is at risk, with no stated workaround.",
        ],
      },
    },
  };
}
function serviceMonitorDocument() {
  if (!setupState.base)
    throw Error("Open a monitoring service before editing.");
  const doc = structuredClone(setupState.base);
  const source = (doc.sources || []).find(
    (s) => s.name === setupState.sourceName,
  );
  const sourceName = source?.name || $("setup-name").value.trim();
  if (!sourceName) throw Error("Name the monitoring service.");
  if (!source && (doc.sources || []).some((s) => s.name === sourceName))
    throw Error(
      "That monitor name is already in use. Choose a new name or edit the existing monitor.",
    );
  let modelName = $("setup-connection").value;
  if (!modelName) {
    modelName = $("setup-connection-name").value.trim();
    if (!modelName) throw Error("Name the new model connection.");
    if (Object.hasOwn(doc.models || {}, modelName))
      throw Error(
        "That model connection already exists. Select it instead, or use a new name.",
      );
    doc.models = {
      ...doc.models,
      [modelName]: {
        role: "decision",
        provider: $("setup-provider").value,
        model: $("setup-model").value.trim(),
        base_url: $("setup-url").value.trim(),
        ...($("setup-env").value.trim()
          ? { api_key_env: $("setup-env").value.trim() }
          : {}),
      },
    };
  }
  const interpret = serviceInterpretation(modelName);
  // Only an empty bootstrap gets a new fallback interpreter. Adding a monitor
  // to a working runtime must not rewrite its webhook or legacy-source rules.
  const fallback = doc.interpret?.model;
  if (
    !doc.sources?.length &&
    doc.models?.[fallback]?.model === "replace-with-your-model"
  ) {
    delete doc.models[fallback];
    doc.interpret = structuredClone(interpret);
    doc.policy = [];
    doc.default = { ignore: true };
  }
  const conditions = { "result.category.choice": "incident" };
  if (doc.models[modelName].provider === "openai-compatible")
    conditions["result.requires_action.value"] = true;
  else
    Object.assign(conditions, {
      "result.category.confidence": `>= ${Number($("setup-confidence").value)}`,
      "result.requires_action.noul": `>= ${Number($("setup-action-probability").value)}`,
    });
  const notify = {
    via: $("setup-notify").value,
    message:
      "Review {{ .event.subject }} — source {{ .event.source }} — event {{ .event.id }}",
  };
  if (notify.via === "discord")
    notify.webhook_env =
      $("setup-webhook").value || $("setup-webhook-env").value.trim();
  doc.monitors = {
    ...doc.monitors,
    [sourceName]: {
      interpret,
      policy: [{ if: conditions, then: { notify, shadow: true } }],
      default: { ignore: true },
    },
  };
  const next = {
    ...source,
    name: sourceName,
    type: "systemd-journal",
    event_type: source?.event_type || "service.warning",
    every: $("setup-every").value.trim(),
    units: $("setup-units")
      .value.split(",")
      .map((s) => s.trim())
      .filter(Boolean),
    priority: $("setup-priority").value,
  };
  doc.sources ||= [];
  if (source) doc.sources[doc.sources.indexOf(source)] = next;
  else doc.sources.push(next);
  return doc;
}
async function buildSetup() {
  await setDocument(serviceMonitorDocument());
  setupState.candidate = $("yaml").value;
  setupState.dirty = false;
}
function setupSummary() {
  const doc = serviceMonitorDocument(),
    name = setupState.sourceName || $("setup-name").value.trim();
  const source = doc.sources.find((s) => s.name === name),
    contract = doc.monitors[name];
  const model = doc.models[contract.interpret.model],
    notify = contract.policy[0].then.notify;
  $("setup-summary").replaceChildren();
  for (const [label, value] of [
    [
      "Monitor",
      `${source.name}${source.paused ? " (paused)" : ""}: ${source.units.join(", ")}`,
    ],
    ["Collection", `${source.priority} and higher, every ${source.every}`],
    [
      "Model",
      `${contract.interpret.model}: ${model.model} at ${model.base_url}`,
    ],
    [
      "Notifications",
      notify.via === "discord"
        ? `Discord via ${notify.webhook_env} · shadow only; delivery untested`
        : "Runtime log · shadow only",
    ],
    [
      "Policy",
      model.provider === "openai-compatible"
        ? "Incident + action needed → shadow notification; otherwise ignore."
        : `Incident + confidence ≥ ${$("setup-confidence").value} + action probability ≥ ${$("setup-action-probability").value} → shadow notification; otherwise ignore.`,
    ],
    [
      "Other work",
      `${doc.sources.length - 1} other sources retained from the starting document. ${doc.filter?.length || 0} shared pre-model filters still apply.`,
    ],
  ])
    $("setup-summary").append(el("dt", label), el("dd", value));
}
function setupAction(id, fn) {
  action(id, async () => {
    $("setup-status").textContent = "";
    notice("");
    try {
      await fn();
    } catch (error) {
      if (id === "setup-test")
        $("setup-test-result").textContent =
          "Test did not complete. Correct the problem below, then retry.";
      $("setup-status").textContent =
        error.name === "AbortError"
          ? "Request timed out. Reload saved state before retrying; activation may have completed."
          : error.message;
      throw error;
    }
  });
}
setupAction("setup-next-source", async () => {
  if (validSetupStep(0)) setupStep(1);
});
setupAction("setup-next-model", async () => {
  if (validSetupStep(1)) {
    notificationFields();
    setupStep(2);
  }
});
setupAction("setup-back-source", async () => setupStep(0));
setupAction("setup-back-model", async () => setupStep(1));
setupAction("setup-back-notify", async () => setupStep(2));
setupAction("setup-next-notify", async () => {
  if (!validSetupStep(2)) return;
  await configurationTask(async () => {
    await buildSetup();
    await validated();
    setupSummary();
    setupStep(3);
  });
});
setupAction("setup-test", async () => {
  if (!setupState.candidate || setupState.candidate !== $("yaml").value)
    throw Error("Draft changed. Go back and review it again.");
  const yaml = $("yaml").value;
  const source = setupState.sourceName || $("setup-name").value.trim();
  invalidateSetupTest();
  $("setup-test-result").textContent =
    "Testing two synthetic examples. No effects will run…";
  await configurationTask(async () => {
    const result = await api("/manage/api/test-model", {
      yaml,
      source,
      confirm: true,
    });
    if ($("yaml").value !== yaml)
      throw Error(
        "Draft changed during the test. Test the current draft again.",
      );
    setupState.test = { yaml, ready: result.ready };
    const report = result.report;
    $("setup-test-result").replaceChildren(
      el(
        "p",
        result.ready
          ? "Connected: the model returned two valid structured answers."
          : "Connection check failed. Check the endpoint, model identity, and credential on the runtime host, then retry.",
      ),
    );
    for (const [i, row] of (report.results || []).entries())
      $("setup-test-result").append(
        el(
          "p",
          `${i === 0 ? "Synthetic incident" : "Synthetic routine event"}: ${row.status === "ok" ? `${row.category} → ${row.outcome} · ${row.latency_ms} ms` : "No valid answer"}.`,
        ),
      );
    if (
      result.ready &&
      (report.category_correct !== 2 ||
        report.requires_action_correct !== 2 ||
        report.outcome_correct !== 2)
    )
      $("setup-test-result").append(
        el(
          "p",
          "The model connected, but an answer or outcome did not match expectations. Inspect this monitor's questions and policy in Advanced configuration. This is not evidence of readiness for live effects.",
        ),
      );
    $("setup-test-result").append(
      el("p", "Real-event quality and Discord delivery remain untested."),
    );
  });
});
setupAction("setup-start", async () => {
  if (
    !setupState.test?.ready ||
    setupState.test.yaml !== $("yaml").value ||
    !$("setup-confirm").checked
  )
    throw Error(
      "Test this exact draft and confirm its collection scope first.",
    );
  await configurationTask(async () => {
    await saveDraft();
    $("apply-confirm").checked = true;
    await activateDraft();
    notice(
      "Monitor configuration is active in shadow. No messages are sent. Check Overview for poll health and Activity for new Events.",
    );
    $("add-monitor").focus();
  });
});
