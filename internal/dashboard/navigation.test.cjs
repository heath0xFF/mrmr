'use strict';

// Exercise the same URL functions shipped to the browser without a DOM library.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, 'dashboard.js'), 'utf8').split('const node =')[0];
const fields = {query: {}, view: {}};
const context = vm.createContext({URLSearchParams, document: {getElementById: id => fields[id]}, location: {pathname: '/dashboard/', search: ''}, history: {pushState() {}}});
vm.runInContext(source, context);
const run = code => vm.runInContext(code, context);

context.location.search = '?q=ssh%20warning&view=errors&offset=50&event=evt%2F123';
assert.equal(run('readLocation()'), 'evt/123');
assert.equal(fields.query.value, 'ssh warning');
assert.equal(fields.view.value, 'errors');
assert.equal(run('locationFor("evt/123")'), '/dashboard/?q=ssh+warning&view=errors&offset=50&event=evt%2F123#activity');

context.location.search = '?view=unknown&offset=100001&q=' + 'x'.repeat(201);
assert.equal(run('readLocation()'), '');
assert.equal(run('state.view'), 'all');
assert.equal(run('state.offset'), 0);
assert.equal(fields.query.value.length, 200);
assert.equal(run('locationFor(null)').includes('event='), false);
context.location.search = '?view=pending';
run('readLocation()');
assert.equal(fields.view.value, 'pending');
assert.match(run('executionSummary({outcome: "notify", status: "pending"})'), /may or may not/);
assert.match(run('executionSummary({outcome: "notify", status: "shadow"})'), /no adapter effect/);
assert.match(run('executionSummary({outcome: "ignore", status: "filtered"})'), /pre-model filter/);
assert.match(run('executionSummary({outcome: "delegate", status: "ok"})'), /does not prove downstream work completed/);
console.log('Dashboard navigation and outcome checks passed.');
