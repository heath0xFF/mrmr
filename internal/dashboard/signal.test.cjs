#!/usr/bin/env node
'use strict';
// The status headline must never claim more than the stored evidence shows.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const context = vm.createContext({console});
vm.runInContext(fs.readFileSync(path.join(__dirname, 'signal.js'), 'utf8'), context);
const Signal = vm.runInContext('Signal', context);

const now = new Date().toISOString();
const reading = (summary, extra = {}) => JSON.parse(JSON.stringify(Signal.assess({
  at: now, started_at: now,
  sources: [{name: 'Plex', event_type: 'x', every_ms: 30000, health: {finished_at: now, last_success: now}}],
  policy: [{rule: 'rule:1', outcome: 'notify', shadow: false}, {rule: 'default', outcome: 'ignore'}],
  summary: {events: 0, models: [], delivered: [], ...summary}, ...extra,
})));
const effect = (r) => r.tracks[0].stages.find((s) => s.label === 'Effect');

let r = reading({});
assert.equal(r.title, 'Watching, no events yet', 'no events must not read as all clear');
assert.equal(r.tone, 'off');
assert.match(r.sentence, /No event has reached the model yet/);
assert.match(r.sentence, /none has sent yet/);
assert.equal(effect(r).state, 'set', 'a live rule alone is configuration, not evidence');
assert.equal(effect(r).detail, 'Live, nothing sent yet');

const ok = {interpreter: '', last_status: 'ok', errors: 0, last_at: now};
r = reading({events: 3, models: [ok]});
assert.equal(r.title, 'All tracks clear');
assert.equal(effect(r).state, 'set', 'events without a send still prove nothing about delivery');

r = reading({events: 3, models: [ok], delivered: [{source: 'Plex', sent: 2, last_sent: now, last_at: now, last_status: 'ok'}]});
assert.equal(effect(r).state, 'clear');
assert.doesNotMatch(r.sentence, /none has sent/);

r = reading({events: 3, models: [ok], delivered: [{source: 'Plex', sent: 2, last_sent: now, last_at: now, last_status: 'error'}]});
assert.equal(effect(r).state, 'caution');
assert.match(r.title, /Plex needs a look at Effect/, 'a failed send must reach the headline');

r = reading({events: 3, models: [ok], delivered: [{source: 'Other', sent: 2, last_sent: now, last_at: now, last_status: 'ok'}]});
assert.equal(effect(r).state, 'set', "another source's send proves nothing for this one");

r = reading({}, {effects_blocked: true});
assert.equal(r.title, 'Effects are stopped by the safety gate', 'a blocked gate keeps its own headline');
console.log('Signal status checks passed.');
