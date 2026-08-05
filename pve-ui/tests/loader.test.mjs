import assert from 'node:assert/strict';
import { webcrypto } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const loaderSource = await readFile(new URL('../pvn-loader.js', import.meta.url), 'utf8');

function harness() {
  const listeners = new Map();
  const frames = [];
  const apiRequests = [];

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
    location: { hostname: 'pve.example.test', origin: 'https://pve.example.test:8006' },
    crypto: webcrypto,
    PVE: { panel: { Config: BaseConfig }, dc: { Config: DatacenterConfig } },
    Proxmox: { Utils: { API2Request(options) { apiRequests.push(options); } } },
    Ext: {
      ClassManager: { get(name) { return classes.get(name); } },
      override(target, methods) { Object.assign(target.prototype, methods); },
    },
    addEventListener(type, callback) { listeners.set(type, callback); },
    removeEventListener(type, callback) { if (listeners.get(type) === callback) listeners.delete(type); },
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
  return { window, DatacenterConfig, frames, listeners, apiRequests };
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
