import assert from 'node:assert/strict';
import { webcrypto } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const loaderSource = await readFile(new URL('../pvn-loader.js', import.meta.url), 'utf8');

function harness(options = {}) {
  const listeners = new Map();
  const frames = [];
  const apiRequests = [];
  const openedWindows = [];

  function BaseConfig() {}
  BaseConfig.prototype.initComponent = function initComponent() { this.initialized = true; };
  function DatacenterConfig() {}
  DatacenterConfig.prototype = Object.create(BaseConfig.prototype);
  DatacenterConfig.prototype.constructor = DatacenterConfig;
  DatacenterConfig.prototype.initComponent = function initComponent() {
    this.items = [{ itemId: 'summary' }];
    BaseConfig.prototype.initComponent.call(this);
  };

  const classes = new Map([
    ['PVE.panel.Config', BaseConfig],
    ['PVE.dc.Config', DatacenterConfig],
  ]);
  const window = {
    location: {
      hostname: options.hostname || 'pve.example.test',
      origin: options.origin || 'https://pve.example.test:8006',
    },
    crypto: webcrypto,
    PVE: { panel: { Config: BaseConfig }, dc: { Config: DatacenterConfig } },
    Proxmox: { Utils: { API2Request(options) { apiRequests.push(options); } } },
    Ext: {
      ClassManager: { get(name) { return classes.get(name); } },
      override(target, methods) { Object.assign(target.prototype, methods); },
    },
    addEventListener(type, callback) { listeners.set(type, callback); },
    removeEventListener(type, callback) { if (listeners.get(type) === callback) listeners.delete(type); },
    open(url, target, features) {
      const popup = { opener: window };
      openedWindows.push({ url, target, features, popup });
      return popup;
    },
    setTimeout(callback) { callback(); return 1; },
  };
  window.window = window;
  const document = {
    createElement(tag) {
      assert.equal(tag, 'iframe');
      const frame = {
        contentWindow: { messages: [], postMessage(message, origin) { this.messages.push({ message, origin }); } },
        style: {},
        setAttribute(name, value) { this[name] = value; },
      };
      frames.push(frame);
      return frame;
    },
  };
  const context = vm.createContext({ window, document, URL, Uint32Array, Object, Array, Number, JSON, Error, String });
  vm.runInContext(loaderSource, context);
  return { window, DatacenterConfig, frames, listeners, apiRequests, openedWindows };
}

test('adds one PVN item to the Datacenter config', () => {
  const { DatacenterConfig } = harness();
  const first = new DatacenterConfig();
  first.initComponent();
  assert.equal(first.items.filter((item) => item.itemId === 'pvn').length, 1);
  const second = new DatacenterConfig();
  second.initComponent();
  assert.equal(second.items.filter((item) => item.itemId === 'pvn').length, 1);
});

test('certificate onboarding opens only the same-node manager in an isolated tab', () => {
  const { DatacenterConfig, openedWindows } = harness();
  const config = new DatacenterConfig();
  config.initComponent();
  const panelConfig = config.items.find((item) => item.itemId === 'pvn');
  const toolbar = panelConfig.dockedItems.find((item) => item.xtype === 'toolbar');
  const help = toolbar.items.find((item) => item && item.xtype === 'tbtext');
  const trust = toolbar.items.find((item) => item && item.itemId === 'pvn-trust-certificate');

  assert.match(help.text, /trust this node's PVN certificate/);
  assert.equal(trust.text, 'Trust local PVN certificate');
  assert.match(trust.tooltip, /protected new tab/);
  trust.handler();

  assert.equal(openedWindows.length, 1);
  assert.equal(openedWindows[0].url, 'https://pve.example.test:8443/');
  assert.equal(openedWindows[0].target, '_blank');
  assert.equal(openedWindows[0].features, 'noopener,noreferrer');
  assert.equal(openedWindows[0].popup.opener, null);
});

test('certificate onboarding brackets a same-node IPv6 target', () => {
  const { DatacenterConfig, openedWindows } = harness({
    hostname: '[2001:db8::10]',
    origin: 'https://[2001:db8::10]:8006',
  });
  const config = new DatacenterConfig();
  config.initComponent();
  const panelConfig = config.items.find((item) => item.itemId === 'pvn');
  const toolbar = panelConfig.dockedItems[0];
  toolbar.items.find((item) => item && item.itemId === 'pvn-trust-certificate').handler();
  assert.equal(openedWindows[0].url, 'https://[2001:db8::10]:8443/');
});

test('reload keeps the nonce-bound iframe URL and sandbox', () => {
  const { DatacenterConfig, frames } = harness();
  const config = new DatacenterConfig();
  config.initComponent();
  const panelConfig = config.items.find((item) => item.itemId === 'pvn');
  panelConfig.listeners.afterrender({ body: { dom: { appendChild() {} } }, on() {}, update() {} });
  const frame = frames[0];
  const originalURL = frame.src;
  frame.src = 'about:blank';

  const reload = panelConfig.dockedItems[0].items.find((item) => item && item.itemId === 'pvn-reload');
  reload.handler();

  assert.equal(frame.src, originalURL);
  assert.equal(frame.sandbox, 'allow-scripts allow-forms allow-downloads allow-same-origin allow-top-navigation-by-user-activation');
  assert.equal(new URL(frame.src).origin, 'https://pve.example.test:8443');
});

test('bridge rejects the wrong origin and permits only an exact QEMU config path', () => {
  const { DatacenterConfig, frames, listeners, apiRequests } = harness();
  const config = new DatacenterConfig();
  config.initComponent();
  const panelConfig = config.items.find((item) => item.itemId === 'pvn');
  const panel = {
    body: { dom: { appendChild() {} } },
    on() {},
    update() {},
  };
  panelConfig.listeners.afterrender(panel);
  const frame = frames[0];
  const url = new URL(frame.src);
  const request = {
    source: 'pvn-ui',
    type: 'pvn:pve:request',
    version: 1,
    nonce: url.searchParams.get('pveBridgeNonce'),
    id: 'request-1',
    method: 'GET',
    path: '/nodes/pve-01/qemu/100/config',
  };
  const onMessage = listeners.get('message');
  onMessage({ origin: 'https://evil.example', source: frame.contentWindow, data: request });
  onMessage({ origin: url.origin, source: {}, data: request });
  onMessage({ origin: url.origin, source: frame.contentWindow, data: { ...request, path: '/nodes/pve-01/storage' } });
  onMessage({ origin: url.origin, source: frame.contentWindow, data: { ...request, params: { full: 1 } } });
  assert.equal(apiRequests.length, 0);
  onMessage({ origin: url.origin, source: frame.contentWindow, data: request });
  assert.equal(apiRequests.length, 1);
  assert.equal(apiRequests[0].url, request.path);
  assert.equal(apiRequests[0].method, 'GET');
});

test('bridge permits read-only QEMU status but never status mutations', () => {
  const { DatacenterConfig, frames, listeners, apiRequests } = harness();
  const config = new DatacenterConfig();
  config.initComponent();
  const panelConfig = config.items.find((item) => item.itemId === 'pvn');
  panelConfig.listeners.afterrender({ body: { dom: { appendChild() {} } }, on() {}, update() {} });
  const frame = frames[0];
  const url = new URL(frame.src);
  const base = {
    source: 'pvn-ui', type: 'pvn:pve:request', version: 1,
    nonce: url.searchParams.get('pveBridgeNonce'),
    path: '/nodes/pve-01/qemu/100/status/current',
  };

  listeners.get('message')({
    origin: url.origin,
    source: frame.contentWindow,
    data: { ...base, id: 'status-read', method: 'GET' },
  });
  assert.equal(apiRequests.length, 1);
  assert.equal(apiRequests[0].url, base.path);
  assert.equal(apiRequests[0].method, 'GET');

  listeners.get('message')({
    origin: url.origin,
    source: frame.contentWindow,
    data: { ...base, id: 'status-write', method: 'PUT', params: { digest: 'a'.repeat(40), delete: 'net0' } },
  });
  assert.equal(apiRequests.length, 1);
});

test('bridge permits only the exact read-only QEMU VM inventory query', () => {
  const { DatacenterConfig, frames, listeners, apiRequests } = harness();
  const config = new DatacenterConfig();
  config.initComponent();
  const panelConfig = config.items.find((item) => item.itemId === 'pvn');
  panelConfig.listeners.afterrender({ body: { dom: { appendChild() {} } }, on() {}, update() {} });
  const frame = frames[0];
  const url = new URL(frame.src);
  const base = {
    source: 'pvn-ui', type: 'pvn:pve:request', version: 1,
    nonce: url.searchParams.get('pveBridgeNonce'),
    path: '/cluster/resources',
  };

  [
    { id: 'inventory-no-filter', method: 'GET' },
    { id: 'inventory-all', method: 'GET', params: { type: 'vm', full: 1 } },
    { id: 'inventory-node', method: 'GET', params: { type: 'node' } },
    { id: 'inventory-write', method: 'PUT', params: { type: 'vm' } },
  ].forEach((request) => listeners.get('message')({
    origin: url.origin,
    source: frame.contentWindow,
    data: { ...base, ...request },
  }));
  assert.equal(apiRequests.length, 0);

  listeners.get('message')({
    origin: url.origin,
    source: frame.contentWindow,
    data: { ...base, id: 'inventory-read', method: 'GET', params: { type: 'vm' } },
  });
  assert.equal(apiRequests.length, 1);
  assert.equal(apiRequests[0].url, '/cluster/resources');
  assert.equal(apiRequests[0].method, 'GET');
  assert.equal(Object.keys(apiRequests[0].params).length, 1);
  assert.equal(apiRequests[0].params.type, 'vm');
});

test('bridge sends a nonce-bound response only to the manager origin', () => {
  const { DatacenterConfig, frames, listeners, apiRequests } = harness();
  const config = new DatacenterConfig();
  config.initComponent();
  const panelConfig = config.items.find((item) => item.itemId === 'pvn');
  panelConfig.listeners.afterrender({ body: { dom: { appendChild() {} } }, on() {}, update() {} });
  const frame = frames[0];
  const url = new URL(frame.src);
  const request = {
    source: 'pvn-ui', type: 'pvn:pve:request', version: 1,
    nonce: url.searchParams.get('pveBridgeNonce'), id: 'request-2', method: 'PUT',
    path: '/nodes/pve-01/qemu/101/config', params: {
      digest: 'a'.repeat(40),
      net1: 'virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=1',
    },
  };
  listeners.get('message')({ origin: url.origin, source: frame.contentWindow, data: request });
  apiRequests[0].success({ result: { data: { digest: 'abc' } } });
  assert.equal(frame.contentWindow.messages.length, 1);
  assert.equal(frame.contentWindow.messages[0].origin, url.origin);
  assert.equal(frame.contentWindow.messages[0].message.nonce, request.nonce);
  assert.deepEqual(frame.contentWindow.messages[0].message.data, { digest: 'abc' });
});

test('PUT rejects arbitrary VM settings and permits one digest-bound NIC action', () => {
  const { DatacenterConfig, frames, listeners, apiRequests } = harness();
  const config = new DatacenterConfig();
  config.initComponent();
  const panelConfig = config.items.find((item) => item.itemId === 'pvn');
  panelConfig.listeners.afterrender({ body: { dom: { appendChild() {} } }, on() {}, update() {} });
  const frame = frames[0];
  const url = new URL(frame.src);
  const base = {
    source: 'pvn-ui', type: 'pvn:pve:request', version: 1,
    nonce: url.searchParams.get('pveBridgeNonce'), method: 'PUT',
    path: '/nodes/pve-01/qemu/102/config',
  };
  const invalidParams = [
    { digest: 'a'.repeat(40), memory: 8192 },
    { net0: 'virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=1' },
    {
      digest: 'a'.repeat(40),
      net0: 'virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=1',
      net1: 'virtio=02:00:00:00:00:02,bridge=br-int,firewall=0,link_down=1',
    },
    { digest: 'a'.repeat(40), net0: 'e1000=02:00:00:00:00:01,bridge=vmbr0' },
    { digest: 'a'.repeat(40), delete: 'memory' },
  ];
  invalidParams.forEach((params, index) => {
    listeners.get('message')({
      origin: url.origin,
      source: frame.contentWindow,
      data: { ...base, id: `invalid-${index}`, params },
    });
  });
  assert.equal(apiRequests.length, 0);

  listeners.get('message')({
    origin: url.origin,
    source: frame.contentWindow,
    data: { ...base, id: 'delete-1', params: { digest: 'b'.repeat(40), delete: 'net2' } },
  });
  assert.equal(apiRequests.length, 1);
  assert.equal(apiRequests[0].params.delete, 'net2');
  assert.equal(apiRequests[0].params.digest, 'b'.repeat(40));
});
