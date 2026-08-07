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
  const loadedRows = [];
  const metaUpdates = [];
  const loadingStates = [];
  const panel = new Overview();
  const store = { loadData(rows) { loadedRows.push(rows); } };
  panel.down = (selector) => selector === '#pvn-overview-grid'
    ? { getStore() { return store; } }
    : { setText(value) { metaUpdates.push(value); } };
  panel.setLoading = (value) => loadingStates.push(value);
  panel.loadOverview();
  assert.equal(apiRequests.length, 1);
  assert.equal(apiRequests[0].url, '/pvn/health');
  assert.equal(apiRequests[0].method, 'GET');
  apiRequests[0].success({ result: { data: {
    cluster: 'lab', status: 'ready', version: 'test', time: '2026-08-07T01:02:03Z',
    database: 'ready', ovn_northbound: 'ready', ovn_southbound: 'ready',
    reconciler: 'ready', default_security_policy: 'ready',
    capacity: { ready: true, online_nodes: ['prox1', 'prox2', 'prox3'] },
  } } });
  assert.equal(loadedRows.at(-1).length, 7);
  assert.deepEqual(JSON.parse(JSON.stringify(loadedRows.at(-1)[0])), {
    section: 'Cluster', component: 'lab', status: 'ready', details: 'PVN test',
  });
  assert.match(loadedRows.at(-1)[6].details, /3 online: prox1, prox2, prox3/);
  assert.match(metaUpdates.at(-1), /lab/);
  assert.match(metaUpdates.at(-1), /Version test/);
  assert.deepEqual(loadingStates, ['Loading PVN status...', false]);
});

test('overview is a native striped ExtJS status grid', () => {
  const { classes } = harness();
  const Overview = classes.get('PVN.panel.Overview');
  const panel = new Overview();
  panel.initComponent();
  assert.equal(panel.layout, 'fit');
  assert.equal(panel.items.length, 1);
  assert.equal(panel.items[0].xtype, 'grid');
  assert.equal(panel.items[0].itemId, 'pvn-overview-grid');
  assert.equal(panel.items[0].viewConfig.stripeRows, true);
  assert.deepEqual(Array.from(panel.items[0].columns, (column) => column.text), [
    'Area', 'Component', 'Status', 'Details',
  ]);
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
  }, fields, null, true, 'ports');
  assert.deepEqual(JSON.parse(JSON.stringify(created)), { network_id: 'network-a', security_group_ids: ['sg-a'] });

  const edited = window.PVN.Utils.formPayload({ name: 'renamed', description: '' }, [
    { name: 'name', required: true }, { name: 'description' },
  ], {
    id: 'uuid', revision: 4, state: 'ready', name: 'old', description: 'old',
    network_name: 'human-only', external: true,
    protocol: 'tcp', binding_status: 'bound', node_id: 'node-a',
  }, false, 'networks');
  assert.deepEqual(JSON.parse(JSON.stringify(edited)), { name: 'renamed', description: '', external: true });
});

test('edit payloads never leak fields owned by another collection', () => {
  const { window } = harness();
  const edited = window.PVN.Utils.formPayload({ name: 'renamed' }, [
    { name: 'name', required: true },
  ], {
    id: 'network-a', revision: 3, name: 'old', mtu: 1400, external: false,
    protocol: 'tcp', security_group_id: 'sg-a', node_id: 'node-a', vmid: 100,
  }, false, 'networks');
  assert.deepEqual(JSON.parse(JSON.stringify(edited)), {
    name: 'renamed', mtu: 1400, external: false,
  });
});

test('clearing an optional numeric edit serializes as the API zero value', () => {
  const { window } = harness();
  const edited = window.PVN.Utils.formPayload({ vlan_id: '' }, [
    { name: 'vlan_id', type: 'number' },
  ], {
    name: 'public-flat', provider_network_id: 'provider-a', network_type: 'flat',
    physical_network: 'provider', vlan_id: 123,
  }, false, 'provider-segments');
  assert.equal(edited.vlan_id, 0);
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

function lifecyclePort(overrides = {}) {
  return {
    id: 'port-a', name: 'web-01', revision: 1, generation: 1,
    state: 'ready', applied_revision: 1, mac_address: '02:00:00:00:00:01',
    binding_status: 'unbound', ...overrides,
  };
}

function lifecycleTarget() {
  return { nodeID: 'node-a', nodeName: 'pve-a', vmid: 100, nic: 'net1' };
}

function fakeLifecycleBridge() {
  const events = [];
  const bridge = {
    config: { digest: 'a'.repeat(40) },
    status: 'running',
    async getQemuConfig() { return { ...this.config }; },
    async getQemuStatus() { return { status: this.status }; },
    async setQemuNIC(_node, _vmid, update) {
      events.push(`set:${update.nic}:${update.linkDown ? 'down' : 'up'}`);
      this.config[update.nic] = `virtio=${update.macAddress},bridge=br-int,firewall=0,link_down=${update.linkDown ? 1 : 0}`;
      this.config.digest = update.linkDown ? 'b'.repeat(40) : 'c'.repeat(40);
    },
    async deleteQemuNIC(_node, _vmid, _digest, nic) {
      events.push(`delete:${nic}`);
      delete this.config[nic];
      this.config.digest = 'd'.repeat(40);
    },
  };
  return { bridge, events };
}

function fakeLifecycleAPI(initial) {
  let current = initial;
  const events = [];
  const api = {
    async getPort() {
      events.push('get-port');
      if (current.binding_status === 'binding') current = lifecyclePort({ ...current, revision: 3, applied_revision: 3, binding_status: 'bound' });
      if (current.binding_status === 'detaching') current = lifecyclePort({ ...current, revision: 6, applied_revision: 6, binding_status: 'unbound' });
      return current;
    },
    async attach(_id, input, revision) {
      events.push(`attach:${revision}:${input.generation}`);
      current = lifecyclePort({
        ...current, revision: revision + 1, generation: input.generation + 1,
        node_id: input.node_id, vmid: input.vmid, nic: input.nic, binding_status: 'binding',
      });
      return current;
    },
    async detach(_id, input, revision) {
      events.push(`detach:${revision}:${input.generation}`);
      current = lifecyclePort({ ...current, revision: revision + 1, binding_status: 'detaching' });
      return current;
    },
  };
  return { api, events, get current() { return current; }, set current(value) { current = value; } };
}

const instantLifecycle = { pollAttempts: 3, pollIntervalMs: 0, sleep: async () => {} };

test('attach keeps a staged NIC down until manager binding is confirmed', async () => {
  const { window } = harness();
  const state = fakeLifecycleAPI(lifecyclePort());
  const { bridge, events } = fakeLifecycleBridge();
  const steps = [];
  const result = await window.PVN.PortLifecycle.attach(
    state.api, bridge, state.current, lifecycleTarget(),
    { ...instantLifecycle, onStep(step) { steps.push(step); } },
  );
  assert.equal(result.binding_status, 'bound');
  assert.deepEqual(events, ['set:net1:down', 'set:net1:up']);
  assert.deepEqual(state.events, ['attach:1:1', 'get-port']);
  assert.deepEqual(steps, [
    'checking-vm', 'staging-nic', 'requesting-binding',
    'waiting-for-binding', 'enabling-nic',
  ]);
});

test('attach keeps the NIC down until the bound revision is realized', async () => {
  const { window } = harness();
  const original = lifecyclePort();
  let current = original;
  let polls = 0;
  const api = {
    async attach(_id, input, revision) {
      current = lifecyclePort({
        ...current, revision: revision + 1, generation: input.generation + 1,
        node_id: input.node_id, vmid: input.vmid, nic: input.nic,
        binding_status: 'binding', applied_revision: revision,
      });
      return current;
    },
    async getPort() {
      polls += 1;
      current = lifecyclePort({
        ...current, revision: 3, applied_revision: polls === 1 ? 2 : 3,
        binding_status: 'bound',
      });
      return current;
    },
  };
  const { bridge, events } = fakeLifecycleBridge();
  const result = await window.PVN.PortLifecycle.attach(
    api, bridge, original, lifecycleTarget(), instantLifecycle,
  );
  assert.equal(result.applied_revision, result.revision);
  assert.equal(polls, 2);
  assert.deepEqual(events, ['set:net1:down', 'set:net1:up']);
});

test('rejected attach deletes only the NIC staged for the selected PVN port', async () => {
  const { window } = harness();
  const original = lifecyclePort();
  const state = fakeLifecycleAPI(original);
  state.api.attach = async () => { throw new Error('forbidden'); };
  const { bridge, events } = fakeLifecycleBridge();
  await assert.rejects(
    window.PVN.PortLifecycle.attach(state.api, bridge, original, lifecycleTarget(), instantLifecycle),
    /forbidden/,
  );
  assert.deepEqual(events, ['set:net1:down', 'delete:net1']);
});

test('ambiguous attach failure leaves the staged NIC link-down', async () => {
  const { window } = harness();
  const original = lifecyclePort();
  const state = fakeLifecycleAPI(original);
  state.api.attach = async () => { throw new Error('connection lost'); };
  state.api.getPort = async () => { throw new Error('manager unavailable'); };
  const { bridge, events } = fakeLifecycleBridge();
  await assert.rejects(
    window.PVN.PortLifecycle.attach(state.api, bridge, original, lifecycleTarget(), instantLifecycle),
    /manager state is unknown.*left link-down/,
  );
  assert.deepEqual(events, ['set:net1:down']);
  assert.match(bridge.config.net1, /link_down=1/);
});

test('attach never changes a pre-existing VM NIC', async () => {
  const { window } = harness();
  const state = fakeLifecycleAPI(lifecyclePort());
  const { bridge, events } = fakeLifecycleBridge();
  bridge.config.net1 = 'virtio=02:00:00:00:00:99,bridge=vmbr0,firewall=1,link_down=0';
  await assert.rejects(
    window.PVN.PortLifecycle.attach(state.api, bridge, state.current, lifecycleTarget(), instantLifecycle),
    /already exists/,
  );
  assert.deepEqual(events, []);
  assert.equal(bridge.config.net1.includes('vmbr0'), true);
});

test('detach disables and unbinds before deleting the owned NIC', async () => {
  const { window } = harness();
  const attached = lifecyclePort({
    revision: 4, generation: 2, node_id: 'node-a', vmid: 100, nic: 'net1', binding_status: 'bound',
  });
  const state = fakeLifecycleAPI(attached);
  const { bridge, events } = fakeLifecycleBridge();
  bridge.config.net1 = 'virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=0';
  const result = await window.PVN.PortLifecycle.detach(
    state.api, bridge, attached, lifecycleTarget(), instantLifecycle,
  );
  assert.equal(result.binding_status, 'unbound');
  assert.deepEqual(events, ['set:net1:down', 'delete:net1']);
  assert.deepEqual(state.events, ['detach:4:2', 'get-port']);
});

test('ambiguous detach failure keeps the owned NIC link-down', async () => {
  const { window } = harness();
  const attached = lifecyclePort({
    revision: 4, generation: 2, node_id: 'node-a', vmid: 100, nic: 'net1', binding_status: 'bound',
  });
  const state = fakeLifecycleAPI(attached);
  state.api.detach = async () => { throw new Error('connection lost'); };
  state.api.getPort = async () => { throw new Error('manager unavailable'); };
  const { bridge, events } = fakeLifecycleBridge();
  bridge.config.net1 = 'virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=0';
  await assert.rejects(
    window.PVN.PortLifecycle.detach(state.api, bridge, attached, lifecycleTarget(), instantLifecycle),
    /manager state is unknown.*left link-down/,
  );
  assert.deepEqual(events, ['set:net1:down']);
  assert.match(bridge.config.net1, /link_down=1/);
});

test('stopped VMs are rejected before any NIC mutation', async () => {
  const { window } = harness();
  const state = fakeLifecycleAPI(lifecyclePort());
  const { bridge, events } = fakeLifecycleBridge();
  bridge.status = 'stopped';
  await assert.rejects(
    window.PVN.PortLifecycle.attach(state.api, bridge, state.current, lifecycleTarget(), instantLifecycle),
    /running QEMU VM/,
  );
  assert.deepEqual(events, []);
});

test('PVN NIC ownership requires exact model, MAC, bridge and firewall', () => {
  const { window } = harness();
  const owned = window.PVN.PortLifecycle.isOwnedPVNNIC;
  assert.equal(owned('virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=1', '02:00:00:00:00:01', true), true);
  assert.equal(owned('virtio=02:00:00:00:00:01,bridge=vmbr0,firewall=0,link_down=1', '02:00:00:00:00:01'), false);
  assert.equal(owned('e1000=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=1', '02:00:00:00:00:01'), false);
  assert.equal(owned('virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,rate=10,link_down=1', '02:00:00:00:00:01'), false);
});

test('native PVE NIC writes are digest-bound and restricted to one netN action', async () => {
  const { apiRequests, window } = harness();
  const operation = window.PVN.PortLifecycle.pveBridge.setQemuNIC('pve-a', 100, {
    digest: 'a'.repeat(40), nic: 'net2', macAddress: '02:00:00:00:00:01', linkDown: true,
  });
  assert.equal(apiRequests.length, 1);
  assert.equal(apiRequests[0].url, '/nodes/pve-a/qemu/100/config');
  assert.equal(apiRequests[0].method, 'PUT');
  assert.deepEqual(Object.keys(apiRequests[0].params).sort(), ['digest', 'net2']);
  assert.equal(apiRequests[0].params.net2, 'virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=1');
  apiRequests[0].success({ result: { data: null } });
  await operation;
  assert.throws(() => window.PVN.PortLifecycle.pveBridge.setQemuNIC('pve-a', 100, {
    digest: 'a'.repeat(40), nic: 'net99', macAddress: '02:00:00:00:00:01', linkDown: true,
  }), /Invalid PVN VM NIC identity/);
});

test('manager port lifecycle calls use the exact PVN payload contract', async () => {
  const { apiRequests, window } = harness();
  const operation = window.PVN.PortLifecycle.managerAPI.attach('port-a', {
    node_id: 'node-a', vmid: 100, nic: 'net1', generation: 2,
  }, 8);
  assert.equal(apiRequests[0].url, '/pvn/ports/port-a/attach');
  assert.equal(apiRequests[0].method, 'POST');
  assert.deepEqual(JSON.parse(apiRequests[0].params.payload), {
    node_id: 'node-a', vmid: 100, nic: 'net1', generation: 2,
  });
  assert.equal(apiRequests[0].params.revision, 8);
  assert.equal(apiRequests[0].params.idempotency_key, '11111111-2222-4333-8444-555555555555');
  apiRequests[0].success({ result: { data: lifecyclePort({ binding_status: 'binding' }) } });
  assert.equal((await operation).binding_status, 'binding');
});

test('loader has no detached web-console transport code', () => {
  assert.equal(loaderSource.includes("'use strict'"), false, 'Ext JS callParent overrides must remain non-strict');
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
