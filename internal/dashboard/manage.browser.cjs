"use strict";

// Optional real-browser regression suite. It uses Chrome's debugging protocol
// and Node's built-in WebSocket, so production needs no frontend dependencies.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const os = require("node:os");
const path = require("node:path");
const { spawn } = require("node:child_process");
const { once } = require("node:events");
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
(async () => {
  const profile = await fs.mkdtemp(path.join(os.tmpdir(), "mrmr-browser-"));
  const chrome = spawn(
    process.env.MRMR_CHROME,
    [
      "--headless",
      "--disable-gpu",
      "--no-first-run",
      "--remote-debugging-port=0",
      `--user-data-dir=${profile}`,
      "about:blank",
    ],
    { stdio: "ignore" },
  );
  let socket;
  try {
    let port;
    for (let i = 0; i < 100; i++) {
      try {
        port = (
          await fs.readFile(path.join(profile, "DevToolsActivePort"), "utf8")
        ).split("\n")[0];
        break;
      } catch {
        await delay(50);
      }
    }
    assert.ok(port, "Chrome debugging endpoint did not start");
    const pages = await (
      await fetch(`http://127.0.0.1:${port}/json/list`)
    ).json();
    socket = new WebSocket(
      pages.find((page) => page.type === "page").webSocketDebuggerUrl,
    );
    await new Promise((resolve, reject) => {
      socket.onopen = resolve;
      socket.onerror = reject;
    });
    let sequence = 0;
    const waiting = new Map(),
      errors = [];
    socket.onmessage = (event) => {
      const msg = JSON.parse(event.data);
      if (msg.id) {
        const job = waiting.get(msg.id);
        if (!job) return;
        waiting.delete(msg.id);
        clearTimeout(job.timer);
        if (msg.error) job.reject(Error(msg.error.message));
        else job.resolve(msg.result);
      } else if (msg.method === "Runtime.exceptionThrown") {
        errors.push(msg.params.exceptionDetails.text);
      }
    };
    const rpc = (method, params = {}) =>
      new Promise((resolve, reject) => {
        const id = ++sequence;
        const timer = setTimeout(() => {
          waiting.delete(id);
          reject(
            Error(
              `Browser command timed out: ${method}; ${String(params.expression || "").slice(0, 120)}`,
            ),
          );
        }, 15000);
        waiting.set(id, { resolve, reject, timer });
        socket.send(JSON.stringify({ id, method, params }));
      });
    const evaluate = async (expression) => {
      const result = await rpc("Runtime.evaluate", {
        expression,
        awaitPromise: true,
        returnByValue: true,
      });
      if (result.exceptionDetails)
        throw Error(
          result.exceptionDetails.exception?.description ||
            result.exceptionDetails.text,
        );
      return result.result.value;
    };
    const until = async (expression) => {
      for (let i = 0; i < 100; i++) {
        if (await evaluate(expression)) return;
        await delay(50);
      }
      throw Error(
        `Timed out: ${expression}; page=${await evaluate("location.href")}; text=${await evaluate("document.body?.innerText.slice(0,500)")}`,
      );
    };
    await rpc("Runtime.enable");
    await rpc("Page.enable");
    await rpc("Page.navigate", {
      url: process.env.MRMR_BROWSER_URL + "/manage/",
    });
    // The form exists before deferred scripts attach its submit handler. Wait
    // for those assets, not just the DOM node, before sending credentials.
    await until(
      "document.readyState==='complete' && !!document.getElementById('login-form')",
    );
    await evaluate(
      `document.getElementById('token').value=${JSON.stringify(process.env.MRMR_BROWSER_TOKEN)};document.getElementById('login-form').requestSubmit()`,
    );
    await until("!document.getElementById('app').hidden");
    for (const width of [390, 1440]) {
      await rpc("Emulation.setDeviceMetricsOverride", {
        width,
        height: 1000,
        deviceScaleFactor: 1,
        mobile: width < 600,
      });
      for (const section of [
        "overview",
        "configure",
        "activity",
        "review",
        "runtime",
      ]) {
        await evaluate(`location.hash=${JSON.stringify(section)}`);
        await delay(80);
        const sizes = await evaluate(
          "({scroll:document.documentElement.scrollWidth,width:innerWidth})",
        );
        assert.ok(
          sizes.scroll <= sizes.width + 1,
          `${section} overflows at ${width}: ${JSON.stringify(sizes)}`,
        );
        if (process.env.MRMR_BROWSER_ARTIFACTS) {
          await fs.mkdir(process.env.MRMR_BROWSER_ARTIFACTS, {
            recursive: true,
          });
          const shot = await rpc("Page.captureScreenshot", {
            format: "png",
            captureBeyondViewport: false,
          });
          await fs.writeFile(
            path.join(
              process.env.MRMR_BROWSER_ARTIFACTS,
              `manage-${section}-${width}.png`,
            ),
            Buffer.from(shot.data, "base64"),
          );
        }
      }
    }
    await evaluate(
      "location.hash='configure';document.getElementById('open-advanced').click();document.querySelector('#configured-models button').click()",
    );
    assert.equal(
      await evaluate("document.getElementById('model-name').value"),
      "judge",
    );
    await evaluate(
      "document.getElementById('model-id').value='browser-model';document.getElementById('model-form').requestSubmit()",
    );
    await until(
      "document.getElementById('configured-models').textContent.includes('browser-model')",
    );
    await rpc("Emulation.setDeviceMetricsOverride", {
      width: 390,
      height: 1000,
      deviceScaleFactor: 1,
      mobile: true,
    });
    assert.ok(
      await evaluate("document.documentElement.scrollWidth<=innerWidth+1"),
      "expanded forms overflow on a phone",
    );
    await evaluate(
      "document.getElementById('rule-value').value='incident';document.getElementById('policy-form').requestSubmit()",
    );
    await until(
      "document.querySelectorAll('#configured-rules .record').length===1",
    );
    await evaluate(
      "document.getElementById('source-name').value='paused-fixture';document.getElementById('source-type').value='http-poller';document.getElementById('source-url').value='http://127.0.0.1:1/feed';document.getElementById('source-form').requestSubmit()",
    );
    await until(
      "document.querySelectorAll('#configured-sources .record').length===1",
    );
    await evaluate(
      "[...document.querySelectorAll('#configured-sources button')].find(b=>b.textContent==='Pause in draft').click()",
    );
    await until(
      "document.getElementById('configured-sources').textContent.includes('Resume in draft')",
    );
    await evaluate(
      "document.getElementById('yaml').value=document.getElementById('yaml').value.replace('Classify this event.','A browser-tested contract.');document.getElementById('yaml').dispatchEvent(new Event('input'));document.getElementById('save-draft').click()",
    );
    await until(
      "document.getElementById('notice').textContent.includes('Draft saved')",
    );
    await evaluate("document.getElementById('show-diff').click()");
    assert.ok(
      await evaluate(
        "document.getElementById('diff-content').textContent.includes('A browser-tested contract.')",
      ),
    );
    await evaluate(
      "document.getElementById('apply-confirm').checked=true;document.getElementById('activate').click()",
    );
    await until(
      "document.getElementById('notice').textContent.includes('configuration is active')",
    );
    await evaluate(
      "[...document.querySelectorAll('#revisions button')].at(-1).click()",
    );
    await until(
      "document.getElementById('notice').textContent.includes('Revision loaded')",
    );
    await evaluate("document.getElementById('save-draft').click()");
    await until(
      "document.getElementById('notice').textContent.includes('Draft saved')",
    );
    await evaluate(
      "document.getElementById('apply-confirm').checked=true;document.getElementById('activate').click()",
    );
    await until(
      "document.getElementById('notice').textContent.includes('configuration is active')",
    );
    assert.ok(
      await evaluate(
        "document.getElementById('yaml').value.includes('Classify this event.')",
      ),
    );
    // First-run placeholders must not look connected. Building the first
    // monitor replaces only the empty bootstrap and keeps a safe fallback.
    await evaluate(`(async()=>{
      const bootstrap=await api('/manage/api/validate',{yaml:template(state.config.bootstrap)});
      loadSetup(bootstrap.document);
      if(document.getElementById('setup-connection').value || document.getElementById('setup-model').value || document.getElementById('setup-url').value)
        throw Error('placeholder was presented as a real connection');
      document.getElementById('setup-name').value='first-monitor';
      document.getElementById('setup-units').value='first.service';
      document.getElementById('setup-connection-name').value='d1';
      document.getElementById('setup-provider').value='openai-compatible';
      document.getElementById('setup-model').value='setup-browser-model';
      document.getElementById('setup-url').value=${JSON.stringify(process.env.MRMR_BROWSER_MODEL_URL)};
      const doc=serviceMonitorDocument();
      if(doc.models.judge || doc.interpret.model!=='d1' || !doc.default.ignore || !doc.monitors['first-monitor'].policy[0].then.shadow)
        throw Error('unsafe bootstrap conversion');
      await api('/manage/api/compose',{document:doc});
    })()`);
    // Paused fixtures keep browser tests portable: no host journal is read.
    // Two sources share an event type so adding a local contract must not
    // rewrite the sibling source or the fallback pipeline.
    await evaluate(`(async()=>{
      const doc=await validated();
      doc.sources=[{name:'existing-monitor',type:'systemd-journal',units:['synthetic.service'],event_type:'service.warning',every:'30s',priority:'warning',paused:true},
        {name:'other-monitor',type:'http-poller',url:${JSON.stringify(process.env.MRMR_BROWSER_SOURCE_URL)},event_type:'service.warning',every:'30s'}];
      window.legacyContract=JSON.stringify({interpret:doc.interpret,policy:doc.policy,default:doc.default,models:doc.models});
      await setDocument(doc);
      window.otherSource=JSON.stringify((await validated()).sources[1]);
      loadSetup(doc,'existing-monitor');
      document.getElementById('setup-next-source').click();
    })()`);
    await until("!document.querySelector('[data-setup-step=\"1\"]').hidden");
    assert.ok(await evaluate("document.getElementById('setup-name').disabled"));
    await evaluate(`
      document.getElementById('setup-connection').value='';
      document.getElementById('setup-connection').dispatchEvent(new Event('change'));
      document.getElementById('setup-connection-name').value='service-model';
      document.getElementById('setup-provider').value='liquid-d1';
      document.getElementById('setup-provider').dispatchEvent(new Event('change'));
      document.getElementById('setup-model').value='setup-browser-model';
      document.getElementById('setup-url').value=${JSON.stringify(process.env.MRMR_BROWSER_MODEL_URL)};
      document.getElementById('setup-env').value='MRMR_BROWSER_MISSING_CREDENTIAL';
      document.getElementById('setup-next-model').click();
    `);
    await until("!document.querySelector('[data-setup-step=\"2\"]').hidden");
    assert.ok(
      await evaluate(
        "document.getElementById('setup-notify').value==='discord'",
      ),
    );
    await evaluate("document.getElementById('setup-next-notify').click()");
    await until("!document.querySelector('[data-setup-step=\"3\"]').hidden");
    assert.ok(
      await evaluate("document.getElementById('setup-start').disabled"),
    );
    assert.ok(
      await evaluate(
        "document.getElementById('setup-summary').textContent.includes('0.85')",
      ),
    );
    await evaluate("document.getElementById('setup-test').click()");
    await until(
      "document.getElementById('setup-status').textContent.includes('credential is missing')",
    );
    assert.ok(
      await evaluate("document.getElementById('setup-start').disabled"),
    );
    await evaluate(`
      document.getElementById('setup-back-notify').click(); document.getElementById('setup-back-model').click();
      document.getElementById('setup-provider').value='openai-compatible'; document.getElementById('setup-provider').dispatchEvent(new Event('change'));
      document.getElementById('setup-env').value='';document.getElementById('setup-model').value='replace-with-your-model';
      document.getElementById('setup-next-model').click();
    `);
    await until("!document.querySelector('[data-setup-step=\"2\"]').hidden");
    await evaluate("document.getElementById('setup-next-notify').click()");
    await until("!document.querySelector('[data-setup-step=\"3\"]').hidden");
    await evaluate("document.getElementById('setup-test').click()");
    await until(
      "document.getElementById('setup-status').textContent.includes('example model')",
    );
    await evaluate(`
      document.getElementById('setup-back-notify').click(); document.getElementById('setup-back-model').click();
      document.getElementById('setup-model').value='setup-browser-model';
      document.getElementById('setup-model').dispatchEvent(new Event('input',{bubbles:true}));
      document.getElementById('setup-next-model').click();
    `);
    await until("!document.querySelector('[data-setup-step=\"2\"]').hidden");
    await evaluate("document.getElementById('setup-next-notify').click()");
    await until("!document.querySelector('[data-setup-step=\"3\"]').hidden");
    await evaluate("document.getElementById('setup-test').click()");
    await until(
      "document.getElementById('setup-test-result').textContent.includes('Connected:')",
    );
    assert.ok(
      await evaluate("document.getElementById('setup-start').disabled"),
      "test success is not activation consent",
    );
    await evaluate(
      `setupStep(0);document.getElementById('setup-units').value='other.service';document.getElementById('setup-units').dispatchEvent(new Event('input',{bubbles:true}));`,
    );
    assert.ok(
      await evaluate(
        "document.getElementById('setup-start').disabled && document.getElementById('setup-test-result').textContent.includes('Not tested')",
      ),
    );
    await evaluate(
      "setupStep(2);document.getElementById('setup-next-notify').click()",
    );
    await until("!document.querySelector('[data-setup-step=\"3\"]').hidden");
    await evaluate("document.getElementById('setup-test').click()");
    await until(
      "document.getElementById('setup-test-result').textContent.includes('Connected:')",
    );
    assert.ok(
      await evaluate(
        "!document.getElementById('setup-summary').textContent.includes('0.85')",
      ),
      "generated answers got invented probability thresholds",
    );
    for (const width of [390, 1440]) {
      await rpc("Emulation.setDeviceMetricsOverride", {
        width,
        height: 1000,
        deviceScaleFactor: 1,
        mobile: width < 600,
      });
      for (const step of [0, 1, 2, 3]) {
        await evaluate(`setupStep(${step},false)`);
        assert.ok(
          await evaluate("document.documentElement.scrollWidth<=innerWidth+1"),
          `guided step ${step} overflows at ${width}`,
        );
        if (process.env.MRMR_BROWSER_ARTIFACTS) {
          const shot = await rpc("Page.captureScreenshot", {
            format: "png",
            captureBeyondViewport: true,
          });
          await fs.writeFile(
            path.join(
              process.env.MRMR_BROWSER_ARTIFACTS,
              `setup-${step}-${width}.png`,
            ),
            Buffer.from(shot.data, "base64"),
          );
        }
      }
    }
    await evaluate(
      "document.getElementById('setup-confirm').checked=true;document.getElementById('setup-confirm').dispatchEvent(new Event('change'))",
    );
    assert.ok(
      await evaluate("!document.getElementById('setup-start').disabled"),
    );
    await evaluate("document.getElementById('setup-start').click()");
    await until(
      "document.getElementById('notice').textContent.includes('Monitor configuration is active')",
    );
    assert.ok(
      await evaluate(
        "document.getElementById('setup-start').disabled && document.getElementById('setup-guide').hidden",
      ),
    );
    assert.ok(
      await evaluate(`(()=>{
      const doc=state.config.document;
      const oldModels={...doc.models}; delete oldModels['service-model'];
      return doc.sources[0].paused && doc.sources[0].name==='existing-monitor'
        && JSON.stringify(doc.sources[1])===window.otherSource
        && JSON.stringify({interpret:doc.interpret,policy:doc.policy,default:doc.default,models:oldModels})===window.legacyContract
        && doc.monitors['existing-monitor'].policy[0].then.notify.via==='discord'
        && !doc.monitors['existing-monitor'].policy[0].then.notify.message.includes('data.message')
        && !doc.monitors['other-monitor'];
    })()`),
      "monitor edit changed another contract or exposed raw logs",
    );
    assert.ok(
      await evaluate(
        "document.querySelectorAll('#active-monitors article').length===2 && document.getElementById('active-monitors').textContent.includes('missing')",
      ),
    );
    for (const width of [390, 1440]) {
      await rpc("Emulation.setDeviceMetricsOverride", {
        width,
        height: 1000,
        deviceScaleFactor: 1,
        mobile: width < 600,
      });
      assert.ok(
        await evaluate("document.documentElement.scrollWidth<=innerWidth+1"),
        "current configuration overflows",
      );
      if (process.env.MRMR_BROWSER_ARTIFACTS) {
        const shot = await rpc("Page.captureScreenshot", {
          format: "png",
          captureBeyondViewport: true,
        });
        await fs.writeFile(
          path.join(
            process.env.MRMR_BROWSER_ARTIFACTS,
            `current-config-${width}.png`,
          ),
          Buffer.from(shot.data, "base64"),
        );
      }
    }
    await evaluate(`(()=>{
      const prior=window.confirm;window.confirm=()=>true;
      [...document.querySelectorAll('#active-monitors article')][1].querySelectorAll('button')[1].click();
      window.confirm=prior;
    })()`);
    await until("!state.configBusy && state.config.document.sources[1].paused");
    assert.ok(
      await evaluate(
        "!!state.config.document.monitors['existing-monitor'] && state.config.document.sources.length===2",
      ),
      "pause changed monitor routing",
    );
    // Adding another monitor reuses a connection without editing it. Its draft
    // must not appear in the current list before successful activation.
    await evaluate("document.getElementById('add-monitor').click()");
    await until("!document.getElementById('setup-guide').hidden");
    await evaluate(`
      document.getElementById('setup-name').value='third-monitor';
      document.getElementById('setup-units').value='third.service';
      document.getElementById('setup-next-source').click();
      document.getElementById('setup-connection').value='service-model';
      document.getElementById('setup-connection').dispatchEvent(new Event('change'));
    `);
    assert.ok(
      await evaluate(
        "document.getElementById('setup-new-connection').hidden && document.getElementById('setup-env').disabled",
      ),
    );
    await evaluate("document.getElementById('setup-next-model').click()");
    await until("!document.querySelector('[data-setup-step=\"2\"]').hidden");
    // Save is available in the guide before testing or activation. Credential
    // enrollment and draft persistence must not start the third source.
    await evaluate(`window.beforeSaveActive=state.config.active;
      document.getElementById('setup-webhook').value='';
      document.getElementById('setup-webhook').dispatchEvent(new Event('change'));
      document.getElementById('setup-webhook-env').value='browser_alerts';
      document.getElementById('setup-webhook-env').dispatchEvent(new Event('input',{bubbles:true}));
      document.getElementById('setup-webhook-url').value='https://discord.com/api/webhooks/123/synthetic_browser_credential';
      document.getElementById('setup-webhook-confirm').checked=true;
      document.getElementById('save-draft').click();`);
    await until(
      "!state.configBusy && !state.dirty && state.config.draft.yaml.includes('third-monitor')",
    );
    assert.ok(
      await evaluate(`document.getElementById('save-draft').checkVisibility()
      && document.getElementById('save-draft').textContent.trim()==='Save configuration'
      && document.getElementById('config-state').textContent.includes('Saved draft')
      && document.getElementById('setup-webhook-url').value===''
      && !document.getElementById('setup-webhook-confirm').checked
      && state.config.active===window.beforeSaveActive
      && !document.getElementById('yaml').value.includes('synthetic_browser_credential')
      && !JSON.stringify(state.config).includes('synthetic_browser_credential')
      && state.config.connections.some(c=>c.name==='browser_alerts' && c.credential.startsWith('configured'))
      && localStorage.length===0 && sessionStorage.length===0
      && document.getElementById('setup-start').disabled`),
    );
    // A rejected URL is cleared too. The last valid stored connection survives.
    await evaluate(`document.getElementById('setup-webhook-url').value='https://evil.invalid/synthetic_invalid_credential';
      document.getElementById('setup-webhook-confirm').checked=true;
      document.getElementById('setup-save-discord').click();`);
    await until(
      "!state.configBusy && document.getElementById('setup-webhook-result').textContent.includes('Save not confirmed')",
    );
    assert.ok(
      await evaluate(
        "document.getElementById('setup-webhook-url').value==='' && !document.getElementById('notice').textContent.includes('synthetic_invalid_credential')",
      ),
    );
    await evaluate("document.getElementById('review-config').click()");
    await until("!document.querySelector('[data-setup-step=\"3\"]').hidden");
    assert.ok(
      await evaluate(`(async()=>{
      const doc=await validated(), active=state.config.document;
      return doc.sources.length===3 && doc.monitors['third-monitor'].interpret.model==='service-model'
        && JSON.stringify(doc.models)===JSON.stringify(active.models)
        && JSON.stringify(doc.monitors['existing-monitor'])===JSON.stringify(active.monitors['existing-monitor']);
    })()`),
    );
    await evaluate("document.getElementById('back-current').click()");
    assert.ok(
      await evaluate(
        "document.querySelectorAll('#active-monitors article').length===2 && !document.getElementById('active-monitors').textContent.includes('third-monitor')",
      ),
    );
    // Renaming via the guide is disallowed; duplicate new identities are refused.
    await evaluate(`(async()=>{
      const doc=await validated(); loadSetup(doc);
      document.getElementById('setup-name').value='existing-monitor';
      let rejected=false; try{serviceMonitorDocument()}catch(e){rejected=e.message.includes('already in use')}
      if(!rejected) throw Error('duplicate monitor name accepted');
      configView('current'); setupState.dirty=false;
    })()`);
    // Restore the original label contract before the existing review journey.
    await evaluate(
      "(()=>{const prior=window.confirm;window.confirm=()=>true;document.getElementById('open-advanced').click();[...document.querySelectorAll('#revisions button')].at(-1).click();window.confirm=prior;})()",
    );
    await until(
      "document.getElementById('notice').textContent.includes('Revision loaded')",
    );
    await evaluate("document.getElementById('save-draft').click()");
    await until(
      "document.getElementById('notice').textContent.includes('Draft saved')",
    );
    await evaluate(
      "document.getElementById('apply-confirm').checked=true;document.getElementById('activate').click()",
    );
    await until(
      "document.getElementById('notice').textContent.includes('configuration is active')",
    );
    // Simply visiting the guide must not replace a custom advanced draft.
    await evaluate(`(async()=>{
      document.getElementById('open-advanced').click();
      const doc=await validated(); doc.interpret.prompt='Keep this custom contract.';
      await setDocument(doc); window.customDraft=document.getElementById('yaml').value;
      document.getElementById('open-guided').click();
    })()`);
    await until("!document.getElementById('setup-guide').hidden");
    await evaluate("document.getElementById('open-advanced').click()");
    await until("!document.getElementById('advanced-config').hidden");
    assert.ok(
      await evaluate(
        "document.getElementById('yaml').value===window.customDraft",
      ),
      "opening the guide overwrote a custom draft",
    );
    // Supply explicit fixture consent without leaving a native modal open.
    await evaluate(
      "(()=>{const prior=window.confirm;window.confirm=()=>true;document.getElementById('reload-config').click();window.confirm=prior;})()",
    );
    await until(
      "document.getElementById('yaml').value.includes('Classify this event.')",
    );
    await evaluate("location.hash='activity'");
    await until("!!document.querySelector('#events a')");
    await evaluate("document.querySelector('#events a').click()");
    await until("!document.getElementById('event-detail').hidden");
    assert.ok(
      await evaluate(
        "document.getElementById('event-detail').textContent.includes('9007199254740993')",
      ),
    );
    assert.equal(await evaluate("window.mrmrInjected"), undefined);
    await evaluate("document.getElementById('close-event').click()");
    assert.ok(await evaluate("document.getElementById('event-detail').hidden"));
    await evaluate("history.back()");
    await until("!document.getElementById('event-detail').hidden");
    await evaluate("document.getElementById('review-this-event').click()");
    await until("!document.getElementById('review-form').hidden");
    assert.equal(
      await evaluate("document.getElementById('category').value"),
      "",
    );
    await evaluate(
      "document.getElementById('review-origin').value='synthetic';document.getElementById('category').value='routine';document.getElementById('requires-action').value='false';document.getElementById('expected-outcome').value='ignore';document.getElementById('review-form').requestSubmit()",
    );
    await until(
      "document.getElementById('notice').textContent.includes('Review saved')",
    );
    // Hold a response after headers arrive. Locking must also reject its late
    // body, or an already-authorized fetch could repopulate private content.
    await evaluate(
      `(()=>{const original=window.fetch.bind(window);window.fetch=async(...args)=>{const response=await original(...args);if(args[0]==='/manage/api/state'){const read=response.json.bind(response);response.json=async()=>{const body=await read();await new Promise(resolve=>window.releasePrivateBody=resolve);return body;};}return response;};document.getElementById('reload-config').click();})()`,
    );
    await until("typeof window.releasePrivateBody==='function'");
    await evaluate(
      "document.getElementById('setup-webhook-url').value='https://discord.com/api/webhooks/123/unsaved_synthetic_credential';document.getElementById('logout').click();window.releasePrivateBody()",
    );
    await until(
      "document.getElementById('notice').textContent.includes('session changed')",
    );
    assert.equal(await evaluate("document.getElementById('yaml').value"), "");
    assert.equal(
      await evaluate("document.getElementById('setup-webhook-url').value"),
      "",
    );
    assert.equal(
      await evaluate("document.getElementById('event-detail').textContent"),
      "",
    );
    assert.deepEqual(errors, []);
    console.log(
      "Management browser journeys passed at phone and desktop widths.",
    );
  } finally {
    if (socket) socket.close();
    const exit = once(chrome, "exit");
    chrome.kill("SIGTERM");
    await Promise.race([exit, delay(2000).then(() => chrome.kill("SIGKILL"))]);
    await fs.rm(profile, { recursive: true, force: true });
  }
})().catch((err) => {
  console.error(err.stack || err.message);
  process.exitCode = 1;
});
