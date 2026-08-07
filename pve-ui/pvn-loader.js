(function pvnLoader(window) {
    'use strict';

    if (window.__pvnLoaderInstalled === true) {
        return;
    }
    window.__pvnLoaderInstalled = true;

    var MAX_WAIT_ATTEMPTS = 120;
    var waitAttempts = 0;

    function installClasses(Ext, Proxmox) {
        if (window.__pvnClassesInstalled === true) {
            return;
        }
        window.__pvnClassesInstalled = true;

        var PVN = window.PVN = window.PVN || {};

        function html(value) {
            return Ext.String.htmlEncode(String(value === undefined || value === null ? '' : value));
        }

        function text(value) {
            return typeof value === 'string' ? value.trim() : '';
        }

        function dataOf(value) {
            return value && value.data ? value.data : value || {};
        }

        function humanName(value, fallback, keys) {
            var data = dataOf(value);
            var candidates = keys || ['name', 'address', 'mac_address', 'management_address'];
            for (var index = 0; index < candidates.length; index += 1) {
                var candidate = text(data[candidates[index]]);
                if (candidate) {
                    return candidate;
                }
            }
            return fallback || 'Unnamed resource';
        }

        function statusRenderer(value) {
            var label = text(value) || 'unknown';
            var normalized = label.toLowerCase();
            var icon = /^(active|ready|bound|complete|completed|success|connected)$/.test(normalized)
                ? 'check good'
                : /^(error|failed|degraded|disabled|blocked|unavailable)$/.test(normalized)
                    ? 'times critical'
                    : /^(pending|creating|updating|deleting|binding|detaching|running|queued)$/.test(normalized)
                        ? 'clock-o warning'
                        : 'question-circle-o';
            return '<i class="fa fa-' + icon.split(' ')[0] + ' ' + icon.split(' ')[1] + '"></i> ' + html(label);
        }

        function booleanRenderer(value) {
            if (value === undefined || value === null || value === '') {
                return '-';
            }
            return value ? 'Yes' : 'No';
        }

        function listRenderer(value) {
            if (!Array.isArray(value) || value.length === 0) {
                return '-';
            }
            return html(value.map(function (item) {
                if (item && typeof item === 'object') {
                    return item.address || item.name || '-';
                }
                return item;
            }).join(', '));
        }

        function apiError(response) {
            var result = response && response.result || {};
            var raw = result.message || result.error || response && (response.htmlStatus || response.statusText) || 'PVN request failed';
            return String(raw).replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim().slice(0, 300) || 'PVN request failed';
        }

        function request(options) {
            Proxmox.Utils.API2Request(options);
        }

        function createStore(collection, extraParams) {
            return Ext.create('Ext.data.Store', {
                model: 'PVN.model.Resource',
                proxy: {
                    type: 'proxmox',
                    url: '/pvn/' + collection,
                    extraParams: extraParams || {},
                },
                sorters: [{ property: 'name', direction: 'ASC' }],
            });
        }

        function refreshStore(store) {
            if (store && typeof store.load === 'function') {
                store.load();
            }
        }

        PVN.Utils = {
            apiError: apiError,
            booleanRenderer: booleanRenderer,
            createStore: createStore,
            dataOf: dataOf,
            html: html,
            humanName: humanName,
            listRenderer: listRenderer,
            request: request,
            statusRenderer: statusRenderer,
        };

        Ext.define('PVN.model.Resource', {
            extend: 'Ext.data.Model',
            idProperty: 'id',
            fields: [
                'id', 'name', 'description', 'state', 'status', 'last_error',
                'revision', 'applied_revision', 'created_at', 'updated_at',
                'network_id', 'subnet_id', 'router_id', 'port_id',
                'security_group_id', 'security_group_ids', 'provider_network_id',
                'default_segment_id', 'node_id', 'pool_id', 'cidr', 'gateway_ip',
                'external_network_id', 'external_subnet_id', 'external_ip_address',
                'fixed_ip_address', 'fixed_ips', 'mac_address', 'management_address',
                'physical_network', 'network_type', 'vlan_id', 'mtu', 'roles',
                'central_role', 'last_seen_at', 'action', 'kind', 'target_kind',
                'target_id', 'target_revision', 'started_at', 'completed_at', 'error',
                'binding_status', 'requested_chassis', 'lsp_name', 'nic', 'vmid',
                'generation', 'protocol', 'direction', 'ethertype', 'remote_cidr',
                'remote_group_id', 'port_range_min', 'port_range_max', 'read_only',
                'managed', 'enable_dhcp', 'enable_snat', 'external', 'shared',
                'stateful', 'admin_state_up', 'enabled', 'action',
            ],
        });

        Ext.define('PVN.grid.Resource', {
            extend: 'Ext.grid.Panel',
            alias: 'widget.pvnResourceGrid',
            border: false,
            collection: '',
            emptyText: 'No resources found.',

            initComponent: function () {
                var me = this;
                me.store = me.store || createStore(me.collection, me.extraParams);

                var search = Ext.create('Ext.form.field.Text', {
                    emptyText: 'Filter',
                    width: 220,
                    enableKeyEvents: true,
                    listeners: {
                        keyup: function (field) {
                            var query = text(field.getValue()).toLowerCase();
                            me.store.clearFilter();
                            if (query) {
                                me.store.filterBy(function (record) {
                                    return JSON.stringify(dataOf(record)).toLowerCase().indexOf(query) !== -1;
                                });
                            }
                        },
                    },
                });

                me.tbar = [{
                    text: 'Refresh',
                    iconCls: 'fa fa-refresh',
                    handler: function () { refreshStore(me.store); },
                }, '->', search];
                me.viewConfig = {
                    deferEmptyText: false,
                    emptyText: '<div class="x-grid-empty">' + html(me.emptyText) + '</div>',
                    stripeRows: true,
                };
                me.listeners = Ext.apply({}, me.listeners || {}, {
                    activate: function () { refreshStore(me.store); },
                });
                if (Proxmox.Utils.monStoreErrors) {
                    Proxmox.Utils.monStoreErrors(me, me.store);
                }
                me.callParent();
            },
        });

        Ext.define('PVN.panel.Overview', {
            extend: 'Ext.panel.Panel',
            alias: 'widget.pvnOverview',
            border: false,
            bodyPadding: 18,
            scrollable: true,

            loadOverview: function () {
                var me = this;
                var body = me.down('#pvn-overview-body');
                body.update('<p><i class="fa fa-spinner fa-pulse"></i> Loading PVN status...</p>');
                request({
                    url: '/pvn/health',
                    method: 'GET',
                    success: function (response) {
                        var health = response && response.result && response.result.data || {};
                        var capacity = health.capacity || {};
                        var components = [
                            ['Control database', health.database],
                            ['OVN Northbound', health.ovn_northbound],
                            ['OVN Southbound', health.ovn_southbound],
                            ['Reconciler', health.reconciler],
                            ['Default security policy', health.default_security_policy],
                            ['Cluster capacity', capacity.ready === true ? 'ready' : capacity.ready === false ? 'degraded' : 'unknown'],
                        ];
                        var rows = components.map(function (component) {
                            return '<tr><td>' + html(component[0]) + '</td><td>' + statusRenderer(component[1]) + '</td></tr>';
                        }).join('');
                        body.update(
                            '<h2 style="margin-top:0">' + html(health.cluster || 'PVN') + '</h2>' +
                            '<p>Version ' + html(health.version || 'unknown') + ' &middot; ' + statusRenderer(health.status) + '</p>' +
                            '<table class="x-grid-item" style="width:100%;max-width:720px"><tbody>' + rows + '</tbody></table>'
                        );
                    },
                    failure: function (response) {
                        body.update('<div class="x-form-invalid-under"><b>PVN status unavailable</b><br>' + html(apiError(response)) + '</div>');
                    },
                });
            },

            initComponent: function () {
                var me = this;
                Ext.apply(me, {
                    tbar: [{
                        text: 'Refresh',
                        iconCls: 'fa fa-refresh',
                        handler: function () { me.loadOverview(); },
                    }],
                    items: [{ xtype: 'component', itemId: 'pvn-overview-body' }],
                    listeners: { activate: function () { me.loadOverview(); } },
                });
                me.callParent();
            },
        });

        function namedColumn(title, fallback, keys, flex) {
            return {
                text: title,
                dataIndex: 'name',
                flex: flex || 1,
                minWidth: 150,
                renderer: function (_value, _meta, record) {
                    return '<b>' + html(humanName(record, fallback, keys)) + '</b>';
                },
            };
        }

        function stateColumn() {
            return { text: 'State', dataIndex: 'state', width: 125, renderer: statusRenderer };
        }

        Ext.define('PVN.panel.Networks', {
            extend: 'Ext.tab.Panel', alias: 'widget.pvnNetworks', border: false,
            items: [{
                xtype: 'pvnResourceGrid', title: 'Networks', collection: 'networks',
                columns: [
                    namedColumn('Network', 'Unnamed network'),
                    { text: 'External', dataIndex: 'external', width: 90, renderer: booleanRenderer },
                    { text: 'MTU', dataIndex: 'mtu', width: 90 },
                    { text: 'Provider', dataIndex: 'provider_network_id', flex: 1, renderer: function (value, _meta, record) { return html(record.get('provider_network_name') || (value ? 'Assigned provider' : '-')); } },
                    stateColumn(),
                ],
            }, {
                xtype: 'pvnResourceGrid', title: 'Subnets', collection: 'subnets',
                columns: [
                    namedColumn('Subnet', 'Unnamed subnet', ['name', 'cidr']),
                    { text: 'Network', dataIndex: 'network_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('network_name') || 'Unavailable network'); } },
                    { text: 'CIDR', dataIndex: 'cidr', width: 150 },
                    { text: 'Gateway', dataIndex: 'gateway_ip', width: 140 },
                    { text: 'DHCP', dataIndex: 'enable_dhcp', width: 80, renderer: booleanRenderer },
                    stateColumn(),
                ],
            }],
        });

        Ext.define('PVN.panel.Routers', {
            extend: 'Ext.tab.Panel', alias: 'widget.pvnRouters', border: false,
            items: [{
                xtype: 'pvnResourceGrid', title: 'Routers', collection: 'routers',
                columns: [
                    namedColumn('Router', 'Unnamed router'),
                    { text: 'External network', dataIndex: 'external_network_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('external_network_name') || '-'); } },
                    { text: 'Gateway IP', dataIndex: 'external_ip_address', width: 150 },
                    { text: 'SNAT', dataIndex: 'enable_snat', width: 80, renderer: booleanRenderer },
                    stateColumn(),
                ],
            }, {
                xtype: 'pvnResourceGrid', title: 'Interfaces', collection: 'router-interfaces',
                columns: [
                    { text: 'Router', dataIndex: 'router_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('router_name') || 'Unavailable router'); } },
                    { text: 'Subnet', dataIndex: 'subnet_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('subnet_name') || record.get('subnet_cidr') || 'Unavailable subnet'); } },
                    { text: 'Port', dataIndex: 'port_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('port_name') || 'Managed logical port'); } },
                    stateColumn(),
                ],
            }],
        });

        Ext.define('PVN.panel.Ports', {
            extend: 'Ext.tab.Panel', alias: 'widget.pvnPorts', border: false,
            items: [{
                xtype: 'pvnResourceGrid', title: 'Ports', collection: 'ports', itemId: 'pvn-port-grid',
                columns: [
                    namedColumn('Port', 'Unnamed port', ['name', 'mac_address']),
                    { text: 'Network', dataIndex: 'network_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('network_name') || 'Unavailable network'); } },
                    { text: 'MAC address', dataIndex: 'mac_address', width: 155 },
                    { text: 'Fixed IPs', dataIndex: 'fixed_ips', flex: 1, renderer: listRenderer },
                    { text: 'VM', dataIndex: 'vmid', width: 130, renderer: function (value, _meta, record) { return value ? html((record.get('vm_name') || 'VM') + ' (' + value + ')') : '-'; } },
                    { text: 'Binding', dataIndex: 'binding_status', width: 125, renderer: statusRenderer },
                    stateColumn(),
                ],
            }, {
                xtype: 'panel', title: 'VM Attachments', itemId: 'pvn-port-attachments',
                bodyPadding: 18, html: '<p>Choose a port to inspect or change its VM attachment.</p>',
            }],
        });

        Ext.define('PVN.grid.FloatingIPs', {
            extend: 'PVN.grid.Resource', alias: 'widget.pvnFloatingIPs', collection: 'floating-ips',
            columns: [
                namedColumn('Floating IP', 'Unallocated address', ['address', 'name']),
                { text: 'Provider', dataIndex: 'provider_network_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('provider_network_name') || 'Unavailable provider'); } },
                { text: 'Fixed IP', dataIndex: 'fixed_ip_address', width: 150 },
                { text: 'Port', dataIndex: 'port_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('port_name') || '-'); } },
                { text: 'Router', dataIndex: 'router_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('router_name') || '-'); } },
                { text: 'Status', dataIndex: 'status', width: 125, renderer: statusRenderer },
            ],
        });

        Ext.define('PVN.panel.SecurityGroups', {
            extend: 'Ext.tab.Panel', alias: 'widget.pvnSecurityGroups', border: false,
            items: [{
                xtype: 'pvnResourceGrid', title: 'Security Groups', collection: 'security-groups',
                columns: [
                    namedColumn('Security group', 'Unnamed security group'),
                    { text: 'Description', dataIndex: 'description', flex: 2, renderer: html },
                    { text: 'Stateful', dataIndex: 'stateful', width: 90, renderer: booleanRenderer },
                    { text: 'Managed', dataIndex: 'managed', width: 90, renderer: booleanRenderer },
                    stateColumn(),
                ],
            }, {
                xtype: 'pvnResourceGrid', title: 'Rules', collection: 'security-group-rules',
                columns: [
                    { text: 'Security group', dataIndex: 'security_group_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('security_group_name') || 'Unavailable security group'); } },
                    { text: 'Direction', dataIndex: 'direction', width: 100 },
                    { text: 'Protocol', dataIndex: 'protocol', width: 100 },
                    { text: 'Ports', dataIndex: 'port_range_min', width: 110, renderer: function (value, _meta, record) { return value ? html(value + '-' + (record.get('port_range_max') || value)) : '-'; } },
                    { text: 'Remote CIDR', dataIndex: 'remote_cidr', width: 150 },
                    { text: 'Action', dataIndex: 'action', width: 90 },
                    stateColumn(),
                ],
            }],
        });

        Ext.define('PVN.panel.ProviderNetworks', {
            extend: 'Ext.tab.Panel', alias: 'widget.pvnProviderNetworks', border: false,
            items: [{
                xtype: 'pvnResourceGrid', title: 'Provider Networks', collection: 'provider-networks',
                columns: [
                    namedColumn('Provider network', 'Unnamed provider network'),
                    { text: 'Shared', dataIndex: 'shared', width: 90, renderer: booleanRenderer },
                    { text: 'Default segment', dataIndex: 'default_segment_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('default_segment_name') || '-'); } },
                    { text: 'Description', dataIndex: 'description', flex: 2, renderer: html },
                    stateColumn(),
                ],
            }, {
                xtype: 'pvnResourceGrid', title: 'Segments', collection: 'provider-segments',
                columns: [
                    namedColumn('Segment', 'Unnamed segment'),
                    { text: 'Provider', dataIndex: 'provider_network_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('provider_network_name') || 'Unavailable provider'); } },
                    { text: 'Type', dataIndex: 'network_type', width: 90 },
                    { text: 'Physical network', dataIndex: 'physical_network', width: 150 },
                    { text: 'VLAN', dataIndex: 'vlan_id', width: 80 },
                    stateColumn(),
                ],
            }],
        });

        Ext.define('PVN.grid.Nodes', {
            extend: 'PVN.grid.Resource', alias: 'widget.pvnNodes', collection: 'nodes',
            columns: [
                namedColumn('Node', 'Unavailable node', ['name', 'management_address']),
                { text: 'Management address', dataIndex: 'management_address', width: 170 },
                { text: 'Roles', dataIndex: 'roles', flex: 1, renderer: listRenderer },
                { text: 'Enabled', dataIndex: 'enabled', width: 90, renderer: booleanRenderer },
                { text: 'Central role', dataIndex: 'central_role', width: 120 },
                { text: 'Last seen', dataIndex: 'last_seen_at', width: 175 },
                stateColumn(),
            ],
        });

        Ext.define('PVN.grid.Operations', {
            extend: 'PVN.grid.Resource', alias: 'widget.pvnOperations', collection: 'operations',
            extraParams: { limit: 100 },
            columns: [
                { text: 'Action', dataIndex: 'action', width: 130 },
                { text: 'Kind', dataIndex: 'target_kind', width: 150 },
                { text: 'Target', dataIndex: 'target_id', flex: 1, renderer: function (_value, _meta, record) { return html(record.get('target_name') || 'Unavailable target'); } },
                { text: 'Revision', dataIndex: 'target_revision', width: 90 },
                { text: 'Status', dataIndex: 'status', width: 125, renderer: statusRenderer },
                { text: 'Started', dataIndex: 'started_at', width: 175 },
                { text: 'Completed', dataIndex: 'completed_at', width: 175 },
                { text: 'Error', dataIndex: 'error', flex: 2, renderer: html },
            ],
        });
    }

    function pvnPanels() {
        return [{
            xtype: 'pvnOverview', itemId: 'pvn', title: 'PVN',
            iconCls: 'fa fa-sitemap', expandedOnInit: true,
        }, {
            xtype: 'pvnNetworks', itemId: 'pvn-networks', title: 'Networks',
            iconCls: 'fa fa-cloud', groups: ['pvn'],
        }, {
            xtype: 'pvnRouters', itemId: 'pvn-routers', title: 'Routers',
            iconCls: 'fa fa-random', groups: ['pvn'],
        }, {
            xtype: 'pvnPorts', itemId: 'pvn-ports', title: 'Ports',
            iconCls: 'fa fa-plug', groups: ['pvn'],
        }, {
            xtype: 'pvnFloatingIPs', itemId: 'pvn-floating-ips', title: 'Floating IPs',
            iconCls: 'fa fa-globe', groups: ['pvn'],
        }, {
            xtype: 'pvnSecurityGroups', itemId: 'pvn-security-groups', title: 'Security Groups',
            iconCls: 'fa fa-shield', groups: ['pvn'],
        }, {
            xtype: 'pvnProviderNetworks', itemId: 'pvn-provider-networks', title: 'Provider Networks',
            iconCls: 'fa fa-exchange', groups: ['pvn'],
        }, {
            xtype: 'pvnNodes', itemId: 'pvn-nodes', title: 'Nodes',
            iconCls: 'fa fa-server', groups: ['pvn'],
        }, {
            xtype: 'pvnOperations', itemId: 'pvn-operations', title: 'Operations',
            iconCls: 'fa fa-tasks', groups: ['pvn'],
        }];
    }

    function hasPVNItem(items) {
        return Array.isArray(items) && items.some(function (item) {
            return item && item.itemId === 'pvn';
        });
    }

    function installMenu() {
        var Ext = window.Ext;
        var PVE = window.PVE;
        var Proxmox = window.Proxmox;
        if (!Ext || !PVE || !Proxmox || !Proxmox.Utils ||
            typeof Proxmox.Utils.API2Request !== 'function' ||
            !Ext.ClassManager || typeof Ext.override !== 'function' ||
            typeof Ext.define !== 'function') {
            return false;
        }
        var baseClass = Ext.ClassManager.get('PVE.panel.Config') || PVE.panel && PVE.panel.Config;
        var dcClass = Ext.ClassManager.get('PVE.dc.Config') || PVE.dc && PVE.dc.Config;
        if (!baseClass || !dcClass || !baseClass.prototype || typeof baseClass.prototype.initComponent !== 'function') {
            return false;
        }

        installClasses(Ext, Proxmox);
        if (baseClass.prototype.__pvnMenuPatched === true) {
            return true;
        }
        var original = baseClass.prototype.initComponent;
        Ext.override(baseClass, {
            initComponent: function () {
                var isDatacenter = this instanceof dcClass || this.$className === 'PVE.dc.Config';
                if (isDatacenter && Array.isArray(this.items) && !hasPVNItem(this.items)) {
                    Array.prototype.push.apply(this.items, pvnPanels());
                }
                return original.apply(this, arguments);
            },
        });
        baseClass.prototype.__pvnMenuPatched = true;
        return true;
    }

    function waitForPVE() {
        if (installMenu()) {
            return;
        }
        waitAttempts += 1;
        if (waitAttempts < MAX_WAIT_ATTEMPTS) {
            window.setTimeout(waitForPVE, 250);
        }
    }

    waitForPVE();
}(window));
