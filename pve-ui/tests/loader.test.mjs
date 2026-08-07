import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const loaderSource = await readFile(new URL('../pvn-loader.js', import.meta.url), 'utf8');

function harness() {
  const apiRequests = [];
  const stores = [];
  const aliases = new Map();

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

  function define(name, definition) {
    function Defined(config = {}) { Object.assign(this, config); }
    Object.assign(Defined.prototype, definition, {
      callParent() {},
      down() { return { update() {} }; },
    });
    Defined.definition = definition;
    classes.set(name, Defined);
    const declared = Array.isArray(definition.alias) ? definition.alias : [definition.alias];
    declared.filter(Boolean).forEach((alias) => aliases.set(alias.replace(/^widget\./, ''), Defined));
    return Defined;
  }

  function makeStore(config) {
    const store = {
      ...config,
      loaded: 0,
      filters: [],
      records: [],
      load() { this.loaded += 1; },
      clearFilter() { this.filters = []; },
      filterBy(callback) { this.filters.push(callback); },
      getRange() { return this.records; },
      getById(id) { return this.records.find((record) => record.id === id || record.data?.id === id); },
      on() {},
    };
    stores.push(store);
    return store;
  }

  const Ext = {
    ClassManager: { get(name) { return classes.get(name); } },
    String: {
      htmlEncode(value) {
        return String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
      },
    },
    apply(target, ...sources) { return Object.assign(target, ...sources); },
    encode: JSON.stringify,
    define,
    override(target, methods) { Object.assign(target.prototype, methods); },
    create(name, config) {
      if (name === 'Ext.data.Store') return makeStore(config);
      const Class = classes.get(name) || aliases.get(name.replace(/^widget\./, ''));
      return Class ? new Class(config) : { ...config };
    },
  };

  const window = {
    Ext,
    crypto: {
      randomUUID() { return '11111111-2222-4333-8444-555555555555'; },
    },
    PVE: { panel: { Config: BaseConfig }, dc: { Config: DatacenterConfig } },
    Proxmox: {
      Utils: {
        API2Request(options) { apiRequests.push(options); },
        monStoreErrors() {},
      },
    },
    navigator: { clipboard: { writeText() {} } },
    setTimeout(callback) { callback(); return 1; },
  };
  window.window = window;
  const context = vm.createContext({ window, Object, Array, JSON, String, RegExp });
  vm.runInContext(loaderSource, context);
  return { apiRequests, classes, DatacenterConfig, stores, window };
}

const expectedPanels = [
  ['pvn', 'pvnOverview', 'PVN'],
  ['pvn-networks', 'pvnNetworks', 'Networks'],
  ['pvn-routers', 'pvnRouters', 'Routers'],
  ['pvn-ports', 'pvnPorts', 'Ports'],
  ['pvn-floating-ips', 'pvnFloatingIPs', 'Floating IPs'],
  ['pvn-security-groups', 'pvnSecurityGroups', 'Security Groups'],
  ['pvn-provider-networks', 'pvnProviderNetworks', 'Provider Networks'],
  ['pvn-nodes', 'pvnNodes', 'Nodes'],
  ['pvn-operations', 'pvnOperations', 'Operations'],
];

test('installs one native PVN root with exactly eight grouped children', () => {
  const { DatacenterConfig } = harness();
  const first = new DatacenterConfig();
  first.initComponent();
  const panels = first.items.filter((item) => item.itemId.startsWith('pvn'));
  assert.deepEqual(
    panels.map(({ itemId, xtype, title }) => [itemId, xtype, title]),
    expectedPanels,
  );
  assert.equal(panels[0].expandedOnInit, true);
  assert.equal(panels[0].groups, undefined);
  panels.slice(1).forEach((panel) => assert.deepEqual(Array.from(panel.groups), ['pvn']));
  assert.equal(panels.some((panel) => panel.itemId === 'pvn-projects'), false);

  const second = new DatacenterConfig();
  second.initComponent();
  assert.equal(second.items.filter((item) => item.itemId === 'pvn').length, 1);
});

test('defines native ExtJS panels and resource grids for every menu entry', () => {
  const { classes } = harness();
  for (const name of [
    'PVN.panel.Overview', 'PVN.panel.Networks', 'PVN.panel.Routers', 'PVN.panel.Ports',
    'PVN.grid.FloatingIPs', 'PVN.panel.SecurityGroups', 'PVN.panel.ProviderNetworks',
    'PVN.grid.Nodes', 'PVN.grid.Operations',
  ]) {
    assert.ok(classes.has(name), `${name} was not defined`);
  }
  assert.equal(classes.has('PVN.panel.Projects'), false);
});

test('resource stores use only the same-origin PVN API2 path', () => {
  const { window } = harness();
  const networks = window.PVN.Utils.createStore('networks');
  const operations = window.PVN.Utils.createStore('operations', { limit: 100 });
  assert.equal(networks.proxy.url, '/pvn/networks');
  assert.deepEqual(operations.proxy.extraParams, { limit: 100 });
  assert.equal(operations.proxy.url, '/pvn/operations');
});

test('overview requests health through Proxmox API2Request', () => {
  const { apiRequests, classes } = harness();
  const Overview = classes.get('PVN.panel.Overview');
  const updates = [];
  const panel = new Overview();
  panel.down = () => ({ update(value) { updates.push(value); } });
  panel.loadOverview();
  assert.equal(apiRequests.length, 1);
  assert.equal(apiRequests[0].url, '/pvn/health');
  assert.equal(apiRequests[0].method, 'GET');
  apiRequests[0].success({ result: { data: { cluster: 'lab', status: 'ready', version: 'test' } } });
  assert.match(updates.at(-1), /lab/);
  assert.match(updates.at(-1), /Version test/);
});

test('human labels never fall back to UUIDs in primary cells', () => {
  const { window } = harness();
  const named = window.PVN.Utils.humanName({ id: 'a980c3d4-330d-4e1f-920a-aaaaabbbbbcc', name: 'edge' }, 'Unnamed');
  const unnamed = window.PVN.Utils.humanName({ id: 'a980c3d4-330d-4e1f-920a-aaaaabbbbbcc' }, 'Unnamed');
  assert.equal(named, 'edge');
  assert.equal(unnamed, 'Unnamed');
});

test('projectless resource schemas expose CRUD without stale project fields', () => {
  const { window } = harness();
  assert.deepEqual(Object.keys(window.PVN.Resources).sort(), [
    'floating-ips', 'networks', 'ports', 'provider-networks', 'provider-segments',
    'router-interfaces', 'routers', 'security-group-rules', 'security-groups', 'subnets',
  ]);
  for (const resource of Object.values(window.PVN.Resources)) {
    for (const field of [...(resource.createFields || []), ...(resource.editFields || [])]) {
      assert.notEqual(field.name, 'project_id');
      assert.notEqual(field.name, 'pool_id');
      assert.notEqual(field.name, 'shared');
    }
  }
  assert.equal(window.PVN.Resources.ports.createAction, 'provision');
  assert.equal(window.PVN.Resources.ports.deleteAction, 'deprovision');
});

test('write contract sends encoded payload, revision, and idempotency key', () => {
  const { window } = harness();
  const params = window.PVN.Utils.writeParams({ name: 'edge', external: false }, 7);
  assert.deepEqual(JSON.parse(params.payload), { name: 'edge', external: false });
  assert.equal(params.revision, 7);
  assert.equal(params.idempotency_key, '11111111-2222-4333-8444-555555555555');
  assert.equal(window.PVN.Utils.resourceURL('networks', 'safe id'), '/pvn/networks/safe%20id');
});

test('form payloads omit empty create options and strip managed edit fields', () => {
  const { window } = harness();
  const fields = window.PVN.Resources.ports.createFields;
  const created = window.PVN.Utils.formPayload({
    name: '', network_id: 'network-a', subnet_id: '', fixed_ip_address: '',
    mac_address: '', security_group_ids: ['sg-a', ''],
  }, fields, null, true);
  assert.deepEqual(JSON.parse(JSON.stringify(created)), { network_id: 'network-a', security_group_ids: ['sg-a'] });

  const edited = window.PVN.Utils.formPayload({ name: 'renamed', description: '' }, [
    { name: 'name', required: true }, { name: 'description' },
  ], {
    id: 'uuid', revision: 4, state: 'ready', name: 'old', description: 'old',
    network_name: 'human-only', external: true,
  }, false);
  assert.deepEqual(JSON.parse(JSON.stringify(edited)), { name: 'renamed', description: '', external: true });
});

test('delete and port deprovision use revision and idempotency params', () => {
  const { apiRequests, window } = harness();
  window.PVN.Utils.removeResource('ports', { id: 'port-a', revision: 9 }, { action: 'deprovision' });
  assert.equal(apiRequests.length, 1);
  assert.equal(apiRequests[0].url, '/pvn/ports/port-a/deprovision');
  assert.equal(apiRequests[0].method, 'DELETE');
  assert.equal(apiRequests[0].params.revision, 9);
  assert.equal(apiRequests[0].params.idempotency_key, '11111111-2222-4333-8444-555555555555');
});

test('catalog labels use resource names and never expose reference UUIDs', () => {
  const { window } = harness();
  const store = window.PVN.Catalog.get('networks');
  store.records = [{ id: 'network-uuid', data: { id: 'network-uuid', name: 'private' } }];
  assert.equal(window.PVN.Catalog.label('networks', 'network-uuid', 'Unavailable network'), 'private');
  assert.equal(window.PVN.Catalog.label('networks', 'missing-uuid', 'Unavailable network'), 'Unavailable network');
});

test('details contain the UUID while primary labels do not', () => {
  const { window } = harness();
  const resource = { id: 'a980c3d4-330d-4e1f-920a-aaaaabbbbbcc', name: 'edge', revision: 3 };
  const rows = window.PVN.Utils.detailRows(resource);
  assert.equal(rows.find((row) => row.field === 'id').value, resource.id);
  assert.equal(window.PVN.Utils.humanName(resource, 'Unnamed'), 'edge');
});

test('loader has no detached web-console transport code', () => {
  for (const forbidden of ['iframe', 'postMessage', ':8443', 'pveBridgeNonce', 'pvn-projects', '/session']) {
    assert.equal(loaderSource.includes(forbidden), false, `loader still contains ${forbidden}`);
  }
  for (const stale of ['project_id', 'pool_id', "'shared'"]) {
    assert.equal(loaderSource.includes(stale), false, `loader still contains stale ${stale}`);
  }
});

test('missing ExtJS waits without changing the page', () => {
  let attempts = 0;
  const window = {
    setTimeout(callback) {
      attempts += 1;
      if (attempts < 120) callback();
      return attempts;
    },
  };
  window.window = window;
  vm.runInContext(loaderSource, vm.createContext({ window, Object, Array, JSON, String, RegExp }));
  assert.equal(attempts, 119);
  assert.equal(window.__pvnLoaderInstalled, true);
});
