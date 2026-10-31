'use strict';
const $ = id => document.getElementById(id);
const state = {before: '', offset: 0, query: '', view: 'all', controller: null, detailController: null, selected: null, timer: null, lastAt: null};
const views = new Set(['all', 'unlabeled', 'shadow', 'ignore', 'pending', 'errors']);
// Search text and event IDs stay in the URL so links and browser history restore context.
function readLocation() {
  const params = new URLSearchParams(location.search), offset = Number(params.get('offset'));
  state.before = params.get('before') || '';
  if (state.before && !Number.isFinite(Date.parse(state.before))) state.before = '';
  state.query = (params.get('q') || '').slice(0, 200);
  state.view = views.has(params.get('view')) ? params.get('view') : 'all';
  state.offset = Number.isInteger(offset) && offset >= 0 && offset <= 100000 ? offset : 0;
  $('query').value = state.query; $('view').value = state.view;
  return (params.get('event') || '').slice(0, 200);
}
function locationFor(event = state.selected) {
  const params = new URLSearchParams();
  if (state.query) params.set('q', state.query);
  if (state.view !== 'all') params.set('view', state.view);
  if (state.offset) params.set('offset', state.offset);
  if (state.before) params.set('before', state.before);
  if (event) params.set('event', event);
  const query = params.toString();
  return `${location.pathname}${query ? `?${query}` : ''}#activity`;
}
function navigate() { history.pushState(null, '', locationFor()); }
// Only describe outcomes supported by the stored execution; missing results
// cannot establish whether an external effect occurred.
function executionSummary(x) {
  if (x.status === 'filtered') return 'Ignored by a pre-model filter; no model decision was required.';
  if (x.status === 'pending') return `${x.outcome} intent is unfinished; the effect may or may not have occurred.`;
  if (x.status === 'shadow') return `${x.outcome} was recorded in shadow; no adapter effect was performed.`;
  if (x.status === 'error') return `${x.outcome} failed; inspect the recorded error and trace.`;
  if (x.status === 'ok' && x.outcome === 'ignore') return 'Ignore was recorded; inspect the Decision and trace for the reason.';
  if (x.status === 'ok') return `${x.outcome}: the adapter reported success. This does not prove downstream work completed.`;
  return `${x.outcome || 'Outcome'}: ${x.status || 'status not recorded'}.`;
}
const node = (tag, text, className) => {
  const el = document.createElement(tag);
  if (text !== undefined) el.textContent = text;
  if (className) el.className = className;
  return el;
};
const date = value => value && !value.startsWith('0001-') ? new Date(value) : null;
const stamp = value => date(value)?.toLocaleString() || 'Not observed';
const ms = value => value == null ? '—' : `${value.toLocaleString()} ms`;
const json = value => JSON.stringify(value, null, 2);
function age(value, now = new Date()) {
  const at = date(value);
  if (!at) return 'not observed';
  const seconds = Math.max(0, Math.floor((now - at) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}
async function get(path, signal) {
  const response = await fetch(`/dashboard/api/${path}`, {signal, cache: 'no-store', credentials: 'same-origin'});
  if (!response.ok) throw new Error(response.status === 404 ? 'Event not found.' : `Runtime returned HTTP ${response.status}.`);
  return response.json();
}
function details(label, value) {
  const el = node('details');
  el.append(node('summary', label), node('pre', json(value)));
  return el;
}
function renderOverview(data) {
  const openKeys = new Set([...document.querySelectorAll('details[data-keep-id][open]')].map(el => el.dataset.keepId));
  const focusedKey = document.activeElement?.closest('[data-keep-id]')?.dataset.keepId;
  const sourceStates = [];
  const s = data.summary;
  state.lastAt = data.at;
  $('host-name').textContent = data.hostname || 'Runtime';
  document.title = `${data.hostname || 'Runtime'} · mrmr`;
  $('connection').className = 'connection live';
  $('connection').textContent = `Runtime answered · ${stamp(data.at)} · refreshes every 10 seconds`;
  $('runtime-evidence').textContent = `Runtime view started ${stamp(data.started_at)}. Poll evidence resets on restart or configuration activation.`;
  $('totals').replaceChildren();
  for (const [value, label, view] of [[s.events, 'stored events', 'all'], [s.unlabeled, 'unlabeled', 'unlabeled'], [s.shadow, 'shadow executions'], [s.ignored, 'ignore executions'], [s.pending, 'pending executions']]) {
    const item = node(view ? 'a' : 'span');
    item.append(node('strong', value), document.createTextNode(` ${label}`));
    if (view) {
      item.href = `${location.pathname}${view === 'all' ? '' : `?view=${view}`}#activity`;
      item.dataset.keepId = `total:${view}`;
      item.addEventListener('click', event => {
        if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
        event.preventDefault(); $('query').value = ''; $('view').value = view; search();
        $('activity').scrollIntoView({block: 'start'});
      });
    }
    $('totals').append(item);
  }
  $('totals').append(node('span', 'All time · includes test events', 'totals-note'));
  $('latency-window').textContent = `Latest ${s.window.toLocaleString()} decisions · percentiles from successful decisions only`;
  $('timings-body').replaceChildren();
  $('model-evidence').replaceChildren();
  for (const m of s.models) {
    const row = node('tr'), title = node('td', m.interpreter);
    row.setAttribute('role', 'row'); title.setAttribute('role', 'cell');
    const name = node('span', m.model.length > 46 ? `${m.model.slice(0, 28)}…${m.model.slice(-12)}` : m.model, 'model-name');
    name.title = m.model; title.append(name);
    const labels = ['Median', 'p95', 'OK / total', 'Errors', 'Timeouts'];
    row.append(title, ...[ms(m.median_ms), ms(m.p95_ms), `${m.successes} / ${m.count}`, m.errors, m.timeouts].map((v, i) => {
      const cell = node('td', v); cell.dataset.label = labels[i]; cell.setAttribute('role', 'cell'); return cell;
    }));
    $('timings-body').append(row);
    const evidence = node('details', undefined, 'model-history'); evidence.dataset.keepId = `model:${m.interpreter}:${m.model}`;
    evidence.append(node('summary', `${m.interpreter}: last ${m.last_status} · ${ms(m.last_ms)} · ${age(m.last_at, new Date(data.at))}`));
    evidence.append(node('p', `${m.model}. Last decision: ${stamp(m.last_at)}. Last success: ${stamp(m.last_success)}. Last error: ${stamp(m.last_error)}.`));
    $('model-evidence').append(evidence);
  }
  if (!s.models.length) emptyRow('timings-body', 6, 'No model decisions yet. Latency will appear after an event is interpreted.');
  $('sources').replaceChildren();
  for (const source of data.sources) {
    const el = node('div', undefined, 'source'), h = source.health;
    const now = new Date(data.at), started = date(h.started_at), finished = date(h.finished_at);
    const threshold = Math.max(30000, source.every_ms * 3);
    let status = 'Waiting for first poll', tone = '';
    if (source.paused) { status = 'Paused · cursor retained'; tone = 'shadow'; }
    else if (h.running) {
      status = started && now - started > threshold ? 'Poll running longer than expected' : 'Poll in progress';
      tone = started && now - started > threshold ? 'shadow' : '';
    } else if (h.failed) { status = 'Last poll failed · check service logs'; tone = 'error'; }
    else if (finished) {
      status = now - finished > threshold ? 'No recent completed poll' : 'Last poll completed';
      tone = now - finished > threshold ? 'shadow' : 'ok';
    }
    if (!source.paused && !started && now - new Date(data.started_at) > threshold) { status = 'First poll not observed'; tone = 'shadow'; }
    sourceStates.push(`${source.name}: ${status.toLowerCase()}`);
    el.append(node('h3', source.name), node('p', status, `state ${tone}`));
    el.append(node('p', `Last success ${age(h.last_success, now)}`, 'help'));
    el.append(node('p', `${source.priority ? `${source.priority} or higher` : source.type} · every ${source.every_ms / 1000}s`, 'help'));
    if (source.type === 'systemd-journal') {
      const scope = node('details'); scope.dataset.keepId = `source:${source.name}`;
      scope.append(node('summary', `${source.units.length} selected units`));
      scope.append(node('p', `Exact system units only; user services excluded. First activation starts at the tail, not historical backfill. Event type: ${source.event_type}. Last poll started: ${stamp(h.started_at)}.`));
      const list = node('ul', undefined, 'units'); source.units.forEach(unit => list.append(node('li', unit)));
      scope.append(list); el.append(scope);
    } else {
      el.append(node('p', `${source.event_type} · last poll started ${stamp(h.started_at)}`, 'help'));
    }
    $('sources').append(el);
  }
  $('source-status').textContent = sourceStates.join(' · ') || 'No pull sources configured; HTTP ingress only.';
  if (!data.sources.length) $('sources').append(node('p', 'No pull sources configured. Events can still arrive through the HTTP API.'));
  $('configuration').replaceChildren();
  $('configuration').append(node('p', `Default interpreter: ${data.interpreter || 'not configured'}`));
  for (const [eventType, model] of Object.entries(data.interpret_overrides || {}).sort(([a], [b]) => a.localeCompare(b))) {
    $('configuration').append(node('p', `${eventType} → ${model}`, 'help'));
  }
  for (const [source, model] of Object.entries(data.monitor_interpreters || {}).sort(([a], [b]) => a.localeCompare(b))) {
    $('configuration').append(node('p', `Monitor ${source} → ${model} (source-local contract)`, 'help'));
  }
  for (const model of data.models || []) {
    const el = node('details'); el.dataset.keepId = `configured-model:${model.name}`;
    el.append(node('summary', `${model.name} · ${model.role}`));
    el.append(node('p', `${model.provider || 'Provider not specified'} · ${model.model || 'Model not specified'}`));
    $('configuration').append(el);
  }
  if (!data.models?.length) $('configuration').append(node('p', 'No models configured.', 'help'));
  const effects = data.policy.filter(rule => rule.outcome !== 'ignore');
  const live = effects.filter(rule => !rule.shadow).length;
  $('policy-summary').textContent = data.effects_blocked ? 'Operator safety gate is active. Effects are suppressed before adapter dispatch, including older in-flight work.' : live ? `${live} effect rule(s) are LIVE. This dashboard cannot change them.` : effects.length ? 'All configured effect rules are shadowed. Nothing leaves the runtime through these rules.' : 'Policy selects ignore only.';
  $('policy-summary').className = live ? 'warning' : '';
  $('policy').replaceChildren();
  for (const rule of data.policy) {
    const el = details(`${rule.rule} → ${rule.outcome}${rule.shadow ? ' (shadow)' : ''}`, rule.conditions || 'Default when no rule matches');
    el.className = 'policy-row'; el.dataset.keepId = rule.rule; $('policy').append(el);
  }
  $('filters').textContent = `${data.filter_count} pre-model filter(s). Model errors always route to ignore. Shadow means recorded, not delivered.`;
  $('repeats').replaceChildren();
  for (const repeat of s.repeats) {
    const el = node('div', undefined, 'repeat'), button = node('button', `${repeat.count} × ${repeat.subject || repeat.source}`);
    button.type = 'button'; button.dataset.keepId = `repeat:${repeat.source}:${repeat.subject}:${repeat.message}`;
    button.addEventListener('click', () => {
      $('query').value = (repeat.subject || repeat.source).slice(0, 200); $('view').value = 'all'; search(); $('activity').scrollIntoView({block: 'start'});
    });
    el.append(button, node('p', repeat.message)); $('repeats').append(el);
  }
  if (!s.repeats.length) $('repeats').append(node('p', 'No repeated messages in this window.', 'help'));
  for (const el of document.querySelectorAll('[data-keep-id]')) {
    if (el.tagName === 'DETAILS') el.open = openKeys.has(el.dataset.keepId);
    if (focusedKey === el.dataset.keepId) (el.querySelector('summary') || el).focus({preventScroll: true});
  }
}
function emptyRow(target, columns, text) {
  const row = node('tr'), cell = node('td', text, 'empty'); cell.colSpan = columns; row.append(cell); $(target).append(row);
}
function renderEvents(page) {
  state.before = page.before || '';
  const currentURL = new URL(location.href);
  if (state.before) currentURL.searchParams.set('before', state.before);
  history.replaceState(null, '', currentURL);
  // Preserve keyboard position when a periodic refresh replaces the row nodes.
  const focused = document.activeElement?.dataset.eventId;
  $('events-body').replaceChildren();
  for (const e of page.events) {
    const row = node('tr'), received = node('td', age(e.ingested_at)); received.title = stamp(e.ingested_at);
    row.setAttribute('role', 'row'); row.classList.toggle('selected', state.selected === e.id);
    const content = node('td'), button = node('a', e.subject || e.type, 'event-open');
    button.href = locationFor(e.id); button.dataset.eventId = e.id;
    if (state.selected === e.id) button.setAttribute('aria-current', 'true');
    button.addEventListener('click', event => {
      if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
      event.preventDefault(); state.selected = e.id; navigate(); openEvent(e.id, true);
    });
    content.append(button, node('span', `${e.source} · ${e.type}`, 'event-source'), node('span', e.message || '(No message field)', 'event-message'));
    const outcome = node('td');
    const label = ['shadow', 'pending', 'error', 'filtered'].includes(e.execution_status) ? `${e.outcome} · ${e.execution_status}` : e.outcome || 'No execution yet';
    outcome.append(node('span', label, `state outcome-state ${e.execution_status === 'shadow' ? 'shadow' : e.execution_status === 'error' ? 'error' : ''}`));
    outcome.append(node('span', `Decision: ${e.decision_status || 'not recorded'}${e.labeled ? ' · labeled' : ' · unlabeled'}`, `secondary ${['errored', 'invalid'].includes(e.decision_status) ? 'state error' : ''}`));
    row.append(received, content, outcome, node('td', ms(e.latency_ms)));
    for (const cell of row.children) cell.setAttribute('role', 'cell');
    $('events-body').append(row);
    if (focused === e.id) button.focus({preventScroll: true});
  }
  if (!page.events.length) emptyRow('events-body', 4, state.query || state.view !== 'all' ? 'No matching events. Change the search or filter.' : 'No events stored yet. Check the source scope and poll status.');
  $('event-status').textContent = 'Select an event to inspect its decision and trace.';
  $('page-label').textContent = page.events.length ? `${state.offset + 1}–${state.offset + page.events.length}` : 'No results';
  $('previous').disabled = state.offset === 0;
  $('next').disabled = !page.more || state.offset >= 100000;
}
async function refresh() {
  clearTimeout(state.timer);
  state.controller?.abort();
  const controller = new AbortController(); state.controller = controller;
  const timeout = setTimeout(() => controller.abort(), 8000);
  $('refresh').disabled = true;
  try {
    const params = new URLSearchParams({q: state.query, view: state.view, offset: state.offset, before: state.before});
    const [overview, page] = await Promise.all([get('overview', controller.signal), get(`events?${params}`, controller.signal)]);
    if (state.controller !== controller) return;
    renderOverview(overview); renderEvents(page);
  } catch (error) {
    if (state.controller !== controller) return;
    $('connection').className = 'connection warning';
    $('connection').textContent = `Live data unavailable. ${error.name === 'AbortError' ? 'Request timed out.' : error.message} Last update: ${stamp(state.lastAt)}. Check the runtime and your private connection, then retry.`;
    $('event-status').textContent = 'Displayed events may be stale. Refresh now to retry.';
  } finally {
    clearTimeout(timeout);
    if (state.controller === controller) {
      $('refresh').disabled = false;
      if (!document.hidden) state.timer = setTimeout(refresh, 10000);
    }
  }
}
function search() {
  state.before = ''; state.offset = 0; state.query = $('query').value.trim(); state.view = $('view').value;
  closeDetail(false); navigate(); refresh();
}
async function openEvent(id, moveFocus = false) {
  state.selected = id; state.detailController?.abort();
  for (const button of $('events-body').querySelectorAll('.event-open')) {
    const selected = button.dataset.eventId === id;
    if (selected) button.setAttribute('aria-current', 'true'); else button.removeAttribute('aria-current');
    button.closest('tr').classList.toggle('selected', selected);
  }
  const controller = new AbortController(); state.detailController = controller;
  const timeout = setTimeout(() => controller.abort(), 8000);
  $('detail').hidden = false; $('detail-title').textContent = `Event ${id}`;
  $('detail-status').textContent = 'Loading stored records…'; $('detail-content').replaceChildren();
  if (moveFocus) { $('detail').focus(); $('detail').scrollIntoView({block: 'start'}); }
  try {
    const ins = await get(`event?id=${encodeURIComponent(id)}`, controller.signal);
    if (state.detailController !== controller) return;
    const root = $('detail-content'), e = ins.event;
    root.append(node('p', `${e.subject || e.type} · ${e.source} · event time ${stamp(e.timestamp)}`));
    root.append(node('h3', 'Recorded result'));
    if (ins.executions?.length) {
      for (const x of ins.executions) root.append(node('p', executionSummary(x)));
    } else {
      root.append(node('p', 'No execution recorded. The event may still be processing or its result may be unavailable.'));
    }
    const parent = e.provenance?.parent_event_id;
    if (parent || ins.child_event_ids?.length) {
      root.append(node('h3', 'Related events'));
      const links = node('ul');
      for (const [label, related] of [['Parent', parent], ...(ins.child_event_ids || []).map(child => ['Child', child])]) {
        if (!related) continue;
        const item = node('li'), link = node('a', `${label}: ${related}`);
        link.href = locationFor(related);
        link.addEventListener('click', event => {
          if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
          event.preventDefault(); state.selected = related; navigate(); openEvent(related, true);
        });
        item.append(link); links.append(item);
      }
      root.append(links);
    }
    root.append(node('h3', 'Original message / event (redacted)'), node('pre', typeof e.data?.message === 'string' ? e.data.message : ins.event_json));
    // The server formats this before JSON numbers enter JavaScript's float64.
    const fullEvent = node('details');
    fullEvent.append(node('summary', 'Full normalized Event'), node('pre', ins.event_json));
    root.append(fullEvent);
    root.append(node('h3', 'Decision → outcome'));
    for (const d of ins.decisions || []) {
      root.append(node('p', `${d.interpreter} · ${d.model} · ${d.status} · ${ms(d.latency_ms)}`));
      root.append(node('pre', json(d.result || {error: d.error || 'No valid model result'})));
    }
    if (!ins.decisions?.length) root.append(node('p', 'No decision recorded. The event may be filtered, in flight, or incomplete.'));
    for (const x of ins.executions || []) {
      root.append(node('p', `${x.policy_rule || 'No policy reference'} → ${x.outcome} · ${x.status}${x.adapter ? ` · ${x.adapter}` : ''}`));
      if (x.policy_hash) root.append(node('p', `Recorded policy revision: ${x.policy_hash}. Its historical definition may not be available.`, 'help'));
      if (x.status === 'shadow') root.append(node('p', 'Recorded only. No adapter effect was performed.'));
      if (x.status === 'pending') root.append(node('p', 'Unfinished intent. This does not prove whether an effect happened.', 'warning'));
      if (x.error) root.append(node('pre', x.error));
    }
    if (!ins.executions?.length) root.append(node('p', 'No execution recorded. Refresh to check for completion.'));
    root.append(node('h3', 'Trace'));
    const trace = node('ol', undefined, 'trace');
    for (const step of ins.trace || []) {
      const item = node('li'); item.append(node('strong', step.stage), node('small', stamp(step.at)), node('span', step.msg)); trace.append(item);
    }
    root.append(trace);
    if (!ins.trace?.length) root.append(node('p', 'No completed trace recorded.'));
    root.append(details('Human label', ins.label || 'Unlabeled. Label through the CLI after review.'));
    $('detail-status').textContent = `Detail snapshot: ${new Date().toLocaleString()}. Refresh this event for later records.`;
  } catch (error) {
    if (state.detailController === controller) $('detail-status').textContent = `${error.name === 'AbortError' ? 'Request timed out.' : error.message} Use Refresh this event to retry.`;
  } finally { clearTimeout(timeout); }
}
$('refresh').addEventListener('click', () => { state.before = ''; state.offset = 0; navigate(); refresh(); });
$('search-form').addEventListener('submit', e => { e.preventDefault(); search(); });
$('view').addEventListener('change', search);
$('previous').addEventListener('click', () => { state.offset = Math.max(0, state.offset - 50); closeDetail(false); navigate(); refresh(); });
$('next').addEventListener('click', () => { state.offset = Math.min(100000, state.offset + 50); closeDetail(false); navigate(); refresh(); });
$('refresh-detail').addEventListener('click', () => { if (state.selected) openEvent(state.selected); });
function closeDetail(moveFocus) {
  state.detailController?.abort(); state.detailController = null; $('detail').hidden = true;
  const button = [...$('events-body').querySelectorAll('.event-open')].find(el => el.dataset.eventId === state.selected);
  if (moveFocus) (button || $('query')).focus();
  state.selected = null;
  if (button) { button.removeAttribute('aria-current'); button.closest('tr').classList.remove('selected'); }
}
$('close-detail').addEventListener('click', () => { closeDetail(true); navigate(); });
// Back and Forward restore the same filter, page, and event without a reload.
window.addEventListener('popstate', () => {
  closeDetail(false);
  const id = readLocation();
  if (id) openEvent(id);
  refresh();
});
// The page owns polling. Hidden pages stop scheduling; returning always gets
// fresh evidence rather than leaving a green status from the previous visit.
document.addEventListener('visibilitychange', () => {
  clearTimeout(state.timer);
  if (!document.hidden) refresh();
  else {
    // Invalidate pending responses too; they must not turn a paused view green.
    state.controller?.abort(); state.controller = null; $('refresh').disabled = false;
    $('connection').className = 'connection warning'; $('connection').textContent = 'Live refresh paused while this page is hidden.';
  }
});
window.addEventListener('pagehide', () => { clearTimeout(state.timer); state.controller?.abort(); state.detailController?.abort(); });
window.addEventListener('pageshow', e => { if (e.persisted) refresh(); });
const initialEvent = readLocation();
if (initialEvent) openEvent(initialEvent);
refresh();
