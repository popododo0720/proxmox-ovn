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

        function idempotencyKey() {
            if (window.crypto && typeof window.crypto.randomUUID === 'function') {
                return window.crypto.randomUUID();
            }
            if (window.crypto && typeof window.crypto.getRandomValues === 'function') {
                var values = new Uint32Array(4);
                window.crypto.getRandomValues(values);
                return Array.prototype.map.call(values, function (value) {
                    return value.toString(16).padStart(8, '0');
                }).join('');
            }
            return 'pvn-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2);
        }

        function resourceURL(collection, id, action) {
            var url = '/pvn/' + collection;
            if (id) {
                url += '/' + encodeURIComponent(id);
            }
            if (action) {
                url += '/' + action;
            }
            return url;
        }

        function writeParams(payload, revision, key) {
            var params = {
                payload: Ext.encode ? Ext.encode(payload) : JSON.stringify(payload),
                idempotency_key: key || idempotencyKey(),
            };
            if (revision !== undefined && revision !== null) {
                params.revision = revision;
            }
            return params;
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

        function removeResource(collection, resource, options) {
            var data = dataOf(resource);
            var action = options && options.action;
            request({
                url: resourceURL(collection, data.id, action),
                method: 'DELETE',
                params: {
                    revision: data.revision,
                    idempotency_key: idempotencyKey(),
                },
                success: options && options.success,
                failure: options && options.failure,
            });
        }

        function detailRows(resource) {
            var data = dataOf(resource);
            var keys = Object.keys(data).sort(function (left, right) {
                if (left === 'name') return -1;
                if (right === 'name') return 1;
                if (left === 'id') return -1;
                if (right === 'id') return 1;
                return left.localeCompare(right);
            });
            return keys.map(function (key) {
                var value = data[key];
                if (value && typeof value === 'object') {
                    try {
                        value = JSON.stringify(value, null, 2);
                    } catch (_error) {
                        value = String(value);
                    }
                }
                return { field: key, value: value === undefined || value === null || value === '' ? '-' : String(value) };
            });
        }

        function editableResource(resource) {
            var data = dataOf(resource);
            var result = {};
            var managed = {
                id: true, revision: true, applied_revision: true, state: true, status: true,
                last_error: true, created_at: true, updated_at: true, read_only: true,
                managed: true, node_id: true, vmid: true, nic: true, binding_status: true,
                requested_chassis: true, lsp_name: true, generation: true,
            };
            Object.keys(data).forEach(function (key) {
                if (!managed[key] && !/_name$/.test(key) && !/_label$/.test(key)) {
                    result[key] = data[key];
                }
            });
            return result;
        }

        function normalizeFieldValue(spec, value) {
            if (spec.type === 'number') {
                return value === '' || value === undefined || value === null ? '' : Number(value);
            }
            if (spec.type === 'checkbox') {
                return value === true || value === 1 || value === '1' || value === 'on';
            }
            if (spec.multiple) {
                if (Array.isArray(value)) {
                    return value.filter(Boolean);
                }
                return text(value) ? String(value).split(',').map(function (item) { return item.trim(); }).filter(Boolean) : [];
            }
            return typeof value === 'string' ? value.trim() : value;
        }

        function formPayload(values, fields, resource, isCreate) {
            var payload = isCreate ? {} : editableResource(resource);
            fields.forEach(function (spec) {
                var value = normalizeFieldValue(spec, values[spec.name]);
                if (isCreate && !spec.required && !spec.includeEmpty &&
                    (value === '' || value === undefined || value === null || Array.isArray(value) && value.length === 0)) {
                    return;
                }
                payload[spec.name] = value;
            });
            return payload;
        }

        PVN.Utils = {
            apiError: apiError,
            booleanRenderer: booleanRenderer,
            createStore: createStore,
            dataOf: dataOf,
            detailRows: detailRows,
            editableResource: editableResource,
            formPayload: formPayload,
            html: html,
            humanName: humanName,
            idempotencyKey: idempotencyKey,
            listRenderer: listRenderer,
            removeResource: removeResource,
            request: request,
            resourceURL: resourceURL,
            statusRenderer: statusRenderer,
            writeParams: writeParams,
        };

        Ext.define('PVN.model.Resource', {
            extend: 'Ext.data.Model',
            idProperty: 'id',
            fields: [
                'id', 'name', 'description', 'state', 'status', 'last_error',
                'revision', 'applied_revision', 'created_at', 'updated_at',
                'network_id', 'subnet_id', 'router_id', 'port_id',
                'security_group_id', 'security_group_ids', 'provider_network_id',
                'default_segment_id', 'node_id', 'cidr', 'gateway_ip',
                'external_network_id', 'external_subnet_id', 'external_ip_address',
                'fixed_ip_address', 'fixed_ips', 'mac_address', 'management_address',
                'physical_network', 'network_type', 'vlan_id', 'mtu', 'roles',
                'central_role', 'last_seen_at', 'action', 'kind', 'target_kind',
                'target_id', 'target_revision', 'started_at', 'completed_at', 'error',
                'binding_status', 'requested_chassis', 'lsp_name', 'nic', 'vmid',
                'generation', 'protocol', 'direction', 'ethertype', 'remote_cidr',
                'remote_group_id', 'port_range_min', 'port_range_max', 'read_only',
                'managed', 'enable_dhcp', 'enable_snat', 'external',
                'stateful', 'admin_state_up', 'enabled', 'action',
                'network_name', 'subnet_name', 'subnet_cidr', 'router_name',
                'port_name', 'security_group_name', 'provider_network_name',
                'default_segment_name', 'external_network_name', 'node_name',
                'vm_name', 'target_name',
                { name: 'pvn_label', convert: function (_value, record) {
                    return humanName(record, 'Unnamed resource', ['name', 'address', 'cidr', 'mac_address', 'management_address']);
                } },
            ],
        });

        var catalogStores = {};
        var catalogWatchers = [];
        PVN.Catalog = {
            get: function (collection) {
                if (!catalogStores[collection]) {
                    var store = createStore(collection);
                    catalogStores[collection] = store;
                    if (store.on) {
                        store.on('load', function () {
                            catalogWatchers.slice().forEach(function (callback) { callback(); });
                        });
                    }
                    refreshStore(store);
                }
                return catalogStores[collection];
            },
            label: function (collection, id, fallback, keys) {
                if (!id) return '-';
                var store = this.get(collection);
                var record = store.getById ? store.getById(id) : null;
                return record ? humanName(record, fallback, keys) : fallback;
            },
            watch: function (callback) {
                catalogWatchers.push(callback);
            },
        };

        function referenceRenderer(collection, fallback, keys) {
            return function (value) {
                return html(PVN.Catalog.label(collection, value, fallback, keys));
            };
        }

        function multiReferenceRenderer(collection, fallback, keys) {
            return function (values) {
                if (!Array.isArray(values) || values.length === 0) return '-';
                return html(values.map(function (id) {
                    return PVN.Catalog.label(collection, id, fallback, keys);
                }).join(', '));
            };
        }

        function operationTargetRenderer(_value, _meta, record) {
            var kind = record.get('target_kind');
            var collections = {
                network: 'networks', subnet: 'subnets', router: 'routers',
                'router-interface': 'router-interfaces', port: 'ports',
                'floating-ip': 'floating-ips', 'security-group': 'security-groups',
                'security-group-rule': 'security-group-rules',
                'provider-network': 'provider-networks', 'provider-segment': 'provider-segments',
                node: 'nodes',
            };
            var collection = collections[kind];
            if (!collection) return html('Unavailable target');
            return html(PVN.Catalog.label(collection, record.get('target_id'), 'Unavailable target',
                kind === 'floating-ip' ? ['address', 'name'] : ['name', 'address', 'mac_address', 'management_address']));
        }

        PVN.Utils.referenceRenderer = referenceRenderer;
        PVN.Utils.multiReferenceRenderer = multiReferenceRenderer;

        Ext.define('PVN.form.ResourceCombo', {
            extend: 'Ext.form.field.ComboBox',
            alias: 'widget.pvnResourceCombo',
            queryMode: 'local',
            valueField: 'id',
            displayField: 'pvn_label',
            forceSelection: true,
            editable: true,

            applyResourceFilter: function () {
                var me = this;
                var dependencyValue;
                if (me.match && me.up && me.up('form')) {
                    var dependency = me.up('form').getForm().findField(me.match.formField);
                    dependencyValue = dependency && dependency.getValue();
                }
                me.store.clearFilter();
                if (me.where || me.match && dependencyValue) {
                    me.store.filterBy(function (record) {
                        var data = dataOf(record);
                        var matchesWhere = !me.where || Object.keys(me.where).every(function (key) {
                            return data[key] === me.where[key];
                        });
                        var matchesDependency = !me.match || !dependencyValue ||
                            data[me.match.resourceField || me.match.formField] === dependencyValue;
                        return matchesWhere && matchesDependency;
                    });
                }
            },

            initComponent: function () {
                var me = this;
                me.store = createStore(me.collection);
                me.listConfig = Ext.apply({
                    emptyText: 'No matching resources',
                    getInnerTpl: function () { return '<b>{pvn_label:htmlEncode}</b>'; },
                }, me.listConfig || {});
                me.listeners = Ext.apply({}, me.listeners || {}, {
                    afterrender: function () {
                        me.applyResourceFilter();
                        refreshStore(me.store);
                        if (me.match && me.up('form')) {
                            var dependency = me.up('form').getForm().findField(me.match.formField);
                            if (dependency && dependency.on) {
                                dependency.on('change', function () {
                                    me.setValue('');
                                    me.applyResourceFilter();
                                });
                            }
                        }
                    },
                });
                me.callParent();
            },
        });

        function fieldConfig(spec) {
            var config = {
                name: spec.name,
                fieldLabel: spec.label,
                allowBlank: !spec.required,
                value: spec.defaultValue,
                emptyText: spec.placeholder,
                minValue: spec.minValue,
                maxValue: spec.maxValue,
            };
            if (spec.type === 'number') {
                config.xtype = 'proxmoxintegerfield';
            } else if (spec.type === 'checkbox') {
                config.xtype = 'proxmoxcheckbox';
                config.uncheckedValue = 0;
            } else if (spec.type === 'select') {
                config.xtype = 'proxmoxKVComboBox';
                config.comboItems = spec.options;
                config.editable = false;
            } else if (spec.type === 'resource') {
                config.xtype = 'pvnResourceCombo';
                config.collection = spec.collection;
                config.where = spec.where;
                config.match = spec.match;
                config.multiSelect = Boolean(spec.multiple);
            } else {
                config.xtype = 'textfield';
            }
            return config;
        }

        Ext.define('PVN.window.ResourceEdit', {
            extend: 'Proxmox.window.Edit',
            width: 520,
            modal: true,

            initComponent: function () {
                var me = this;
                var isCreate = !me.resource;
                var fields = isCreate ? me.createFields || [] : me.editFields || [];
                var resource = dataOf(me.resource);
                var collectionPath = me.createAction ? me.collection + '/' + me.createAction : me.collection;
                me.url = isCreate ? resourceURL(collectionPath) : resourceURL(me.collection, resource.id);
                me.method = isCreate ? 'POST' : 'PUT';
                me.isCreate = isCreate;
                me.isAdd = isCreate;
                me.subject = me.subject || me.resourceLabel || 'PVN resource';

                var panel = Ext.create('Proxmox.panel.InputPanel', {
                    items: fields.map(fieldConfig),
                    onGetValues: function (values) {
                        var payload = formPayload(values, fields, resource, isCreate);
                        return writeParams(payload, isCreate ? undefined : resource.revision);
                    },
                });
                me.items = [panel];
                me.callParent();
                if (!isCreate) {
                    me.setValues(resource);
                }
            },
        });

        Ext.define('PVN.window.Details', {
            extend: 'Ext.window.Window',
            width: 720,
            height: 520,
            modal: true,
            layout: 'fit',

            initComponent: function () {
                var me = this;
                var data = dataOf(me.resource);
                var store = Ext.create('Ext.data.Store', {
                    fields: ['field', 'value'],
                    data: detailRows(data),
                });
                Ext.apply(me, {
                    title: humanName(data, me.resourceLabel || 'PVN resource') + ' - Details',
                    items: [{
                        xtype: 'grid', store: store, border: false,
                        columns: [{ text: 'Field', dataIndex: 'field', width: 210 }, {
                            text: 'Value', dataIndex: 'value', flex: 1,
                            renderer: function (value) { return '<pre style="white-space:pre-wrap;margin:0">' + html(value) + '</pre>'; },
                        }],
                    }],
                    buttons: [{
                        text: 'Copy UUID',
                        disabled: !data.id,
                        handler: function () {
                            if (Proxmox.Utils.copyToClipboard) {
                                Proxmox.Utils.copyToClipboard(data.id);
                            } else if (window.navigator && window.navigator.clipboard) {
                                window.navigator.clipboard.writeText(data.id);
                            }
                        },
                    }, { text: 'Close', handler: function () { me.close(); } }],
                });
                me.callParent();
            },
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

                var selectionModel = Ext.create('Ext.selection.RowModel', {});
                var createButton;
                var editButton;
                var deleteButton;
                var detailsButton;

                function selectedRecord() {
                    var selection = selectionModel.getSelection ? selectionModel.getSelection() : [];
                    return selection[0];
                }

                function reload() {
                    refreshStore(me.store);
                }

                function showFailure(response) {
                    Ext.Msg.alert('Error', html(apiError(response)));
                }

                function openEditor(record) {
                    var win = Ext.create('PVN.window.ResourceEdit', {
                        collection: me.collection,
                        resource: record || null,
                        resourceLabel: me.resourceLabel,
                        createFields: me.createFields,
                        editFields: me.editFields,
                        createAction: me.createAction,
                    });
                    if (win.on) win.on('destroy', reload);
                    win.show();
                }

                function openDetails(record) {
                    record = record || selectedRecord();
                    if (!record) return;
                    Ext.create('PVN.window.Details', {
                        resource: dataOf(record),
                        resourceLabel: me.resourceLabel,
                    }).show();
                }

                var toolbar = [{
                    text: 'Refresh',
                    iconCls: 'fa fa-refresh',
                    handler: reload,
                }];
                if (Array.isArray(me.createFields)) {
                    createButton = Ext.create('Ext.button.Button', {
                        text: 'Add', iconCls: 'fa fa-plus', handler: function () { openEditor(null); },
                    });
                    toolbar.unshift(createButton);
                }
                if (Array.isArray(me.editFields) && me.editFields.length > 0) {
                    editButton = Ext.create('Ext.button.Button', {
                        text: 'Edit', iconCls: 'fa fa-pencil', disabled: true,
                        handler: function () { openEditor(selectedRecord()); },
                    });
                    toolbar.splice(createButton ? 1 : 0, 0, editButton);
                }
                if (me.allowDelete) {
                    deleteButton = Ext.create('Ext.button.Button', {
                        text: 'Remove', iconCls: 'fa fa-trash-o', disabled: true,
                        handler: function () {
                            var record = selectedRecord();
                            if (!record) return;
                            var label = humanName(record, me.resourceLabel || 'resource');
                            Ext.Msg.confirm('Confirm removal', 'Remove ' + html(label) + '?', function (answer) {
                                if (answer !== 'yes') return;
                                removeResource(me.collection, record, {
                                    action: me.deleteAction,
                                    success: reload,
                                    failure: showFailure,
                                });
                            });
                        },
                    });
                    toolbar.splice((createButton ? 1 : 0) + (editButton ? 1 : 0), 0, deleteButton);
                }
                detailsButton = Ext.create('Ext.button.Button', {
                    text: 'Details', iconCls: 'fa fa-list-alt', disabled: true,
                    handler: function () { openDetails(); },
                });
                toolbar.push(detailsButton);

                if (selectionModel.on) {
                    selectionModel.on('selectionchange', function (_model, selected) {
                        var record = selected && selected[0];
                        var readOnly = !record || Boolean(record.get ? record.get('read_only') : dataOf(record).read_only);
                        if (editButton) editButton.setDisabled(readOnly);
                        if (deleteButton) deleteButton.setDisabled(readOnly);
                        detailsButton.setDisabled(!record);
                    });
                }

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

                toolbar.push('->', search);
                me.tbar = toolbar;
                me.selModel = selectionModel;
                me.viewConfig = {
                    deferEmptyText: false,
                    emptyText: '<div class="x-grid-empty">' + html(me.emptyText) + '</div>',
                    stripeRows: true,
                };
                me.listeners = Ext.apply({}, me.listeners || {}, {
                    activate: reload,
                    itemdblclick: function (_view, record) { openDetails(record); },
                });
                PVN.Catalog.watch(function () {
                    var view = me.getView && me.getView();
                    if (view && view.refresh) view.refresh();
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

        var networkCreateFields = [
            { name: 'name', label: 'Name', required: true, placeholder: 'application' },
            { name: 'mtu', label: 'Guest MTU', type: 'number', defaultValue: 1400, minValue: 576, maxValue: 9000 },
            { name: 'external', label: 'Provider-backed external network', type: 'checkbox' },
            { name: 'provider_network_id', label: 'Provider network', type: 'resource', collection: 'provider-networks' },
            { name: 'description', label: 'Description' },
        ];
        var networkEditFields = [
            { name: 'name', label: 'Name', required: true },
            { name: 'description', label: 'Description' },
            { name: 'mtu', label: 'Guest MTU', type: 'number', required: true, minValue: 576, maxValue: 9000 },
            { name: 'external', label: 'Provider-backed external network', type: 'checkbox' },
            { name: 'provider_network_id', label: 'Provider network', type: 'resource', collection: 'provider-networks', includeEmpty: true },
        ];
        var subnetCreateFields = [
            { name: 'name', label: 'Name', required: true, placeholder: 'application-v4' },
            { name: 'network_id', label: 'Network', type: 'resource', collection: 'networks', required: true },
            { name: 'cidr', label: 'IPv4 CIDR', required: true, placeholder: '10.42.0.0/24' },
            { name: 'gateway_ip', label: 'Gateway IP', placeholder: '10.42.0.1' },
            { name: 'enable_dhcp', label: 'Enable OVN DHCP', type: 'checkbox', defaultValue: true },
        ];
        var routerCreateFields = [
            { name: 'name', label: 'Name', required: true, placeholder: 'edge' },
            { name: 'description', label: 'Description' },
            { name: 'external_network_id', label: 'External network', type: 'resource', collection: 'networks', where: { external: true } },
            { name: 'external_subnet_id', label: 'External subnet', type: 'resource', collection: 'subnets', match: { formField: 'external_network_id', resourceField: 'network_id' } },
            { name: 'external_ip_address', label: 'Router external IPv4', placeholder: '203.0.113.10' },
            { name: 'enable_snat', label: 'Enable SNAT', type: 'checkbox', defaultValue: true },
        ];
        var routerInterfaceFields = [
            { name: 'router_id', label: 'Router', type: 'resource', collection: 'routers', required: true },
            { name: 'subnet_id', label: 'Subnet', type: 'resource', collection: 'subnets', required: true },
        ];
        var portProvisionFields = [
            { name: 'name', label: 'Name', placeholder: 'web-01' },
            { name: 'network_id', label: 'Tenant network', type: 'resource', collection: 'networks', required: true },
            { name: 'subnet_id', label: 'Subnet', type: 'resource', collection: 'subnets', match: { formField: 'network_id', resourceField: 'network_id' } },
            { name: 'fixed_ip_address', label: 'Requested fixed IPv4' },
            { name: 'mac_address', label: 'Requested MAC' },
            { name: 'security_group_ids', label: 'Security groups', type: 'resource', collection: 'security-groups', multiple: true },
        ];
        var floatingIPCreateFields = [
            { name: 'provider_network_id', label: 'Provider network', type: 'resource', collection: 'provider-networks', required: true },
            { name: 'address', label: 'Floating IPv4', required: true, placeholder: '203.0.113.20' },
            { name: 'router_id', label: 'Router', type: 'resource', collection: 'routers' },
            { name: 'port_id', label: 'Port', type: 'resource', collection: 'ports' },
            { name: 'fixed_ip_address', label: 'Fixed IPv4' },
        ];
        var securityGroupCreateFields = [
            { name: 'name', label: 'Name', required: true, placeholder: 'web-servers' },
            { name: 'description', label: 'Description' },
        ];
        var securityRuleCreateFields = [
            { name: 'security_group_id', label: 'Security group', type: 'resource', collection: 'security-groups', required: true },
            { name: 'direction', label: 'Direction', type: 'select', options: [['ingress', 'Ingress'], ['egress', 'Egress']], defaultValue: 'ingress', required: true },
            { name: 'ethertype', label: 'Ethertype', type: 'select', options: [['IPv4', 'IPv4']], defaultValue: 'IPv4', required: true },
            { name: 'protocol', label: 'Protocol', type: 'select', options: [['', 'Any'], ['tcp', 'TCP'], ['udp', 'UDP'], ['icmp', 'ICMP']] },
            { name: 'port_range_min', label: 'Minimum port', type: 'number', minValue: 1, maxValue: 65535 },
            { name: 'port_range_max', label: 'Maximum port', type: 'number', minValue: 1, maxValue: 65535 },
            { name: 'remote_cidr', label: 'Remote CIDR', placeholder: '10.0.0.0/8' },
            { name: 'remote_group_id', label: 'Remote security group', type: 'resource', collection: 'security-groups' },
            { name: 'action', label: 'Action', type: 'select', options: [['allow', 'Allow'], ['drop', 'Drop']], defaultValue: 'allow', required: true },
            { name: 'description', label: 'Description' },
        ];
        var providerNetworkCreateFields = [
            { name: 'name', label: 'Name', required: true, placeholder: 'public' },
            { name: 'description', label: 'Description' },
        ];
        var providerNetworkEditFields = [
            providerNetworkCreateFields[0],
            providerNetworkCreateFields[1],
            { name: 'default_segment_id', label: 'Default segment', type: 'resource', collection: 'provider-segments', includeEmpty: true },
        ];
        var providerSegmentCreateFields = [
            { name: 'name', label: 'Name', required: true, placeholder: 'public-vlan' },
            { name: 'provider_network_id', label: 'Provider network', type: 'resource', collection: 'provider-networks', required: true },
            { name: 'network_type', label: 'Network type', type: 'select', options: [['vlan', 'VLAN'], ['flat', 'Flat']], defaultValue: 'vlan', required: true },
            { name: 'physical_network', label: 'Physical network', required: true, placeholder: 'provider' },
            { name: 'vlan_id', label: 'VLAN ID', type: 'number', minValue: 1, maxValue: 4094 },
        ];

        PVN.Resources = {
            networks: { createFields: networkCreateFields, editFields: networkEditFields },
            subnets: { createFields: subnetCreateFields, editFields: [subnetCreateFields[0], subnetCreateFields[3], subnetCreateFields[4]] },
            routers: { createFields: routerCreateFields, editFields: routerCreateFields },
            'router-interfaces': { createFields: routerInterfaceFields },
            ports: { createFields: portProvisionFields, createAction: 'provision', deleteAction: 'deprovision' },
            'floating-ips': { createFields: floatingIPCreateFields, editFields: floatingIPCreateFields.slice(2) },
            'security-groups': { createFields: securityGroupCreateFields, editFields: securityGroupCreateFields },
            'security-group-rules': { createFields: securityRuleCreateFields, editFields: securityRuleCreateFields.slice(1) },
            'provider-networks': { createFields: providerNetworkCreateFields, editFields: providerNetworkEditFields },
            'provider-segments': { createFields: providerSegmentCreateFields, editFields: [providerSegmentCreateFields[0]].concat(providerSegmentCreateFields.slice(2)) },
        };

        Ext.define('PVN.panel.Networks', {
            extend: 'Ext.tab.Panel', alias: 'widget.pvnNetworks', border: false,
            items: [{
                xtype: 'pvnResourceGrid', title: 'Networks', collection: 'networks',
                resourceLabel: 'Network', createFields: networkCreateFields,
                editFields: networkEditFields, allowDelete: true,
                columns: [
                    namedColumn('Network', 'Unnamed network'),
                    { text: 'External', dataIndex: 'external', width: 90, renderer: booleanRenderer },
                    { text: 'MTU', dataIndex: 'mtu', width: 90 },
                    { text: 'Provider', dataIndex: 'provider_network_id', flex: 1, renderer: referenceRenderer('provider-networks', 'Unavailable provider') },
                    stateColumn(),
                ],
            }, {
                xtype: 'pvnResourceGrid', title: 'Subnets', collection: 'subnets',
                resourceLabel: 'Subnet', createFields: subnetCreateFields,
                editFields: PVN.Resources.subnets.editFields, allowDelete: true,
                columns: [
                    namedColumn('Subnet', 'Unnamed subnet', ['name', 'cidr']),
                    { text: 'Network', dataIndex: 'network_id', flex: 1, renderer: referenceRenderer('networks', 'Unavailable network') },
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
                resourceLabel: 'Router', createFields: routerCreateFields,
                editFields: routerCreateFields, allowDelete: true,
                columns: [
                    namedColumn('Router', 'Unnamed router'),
                    { text: 'External network', dataIndex: 'external_network_id', flex: 1, renderer: referenceRenderer('networks', 'Unavailable network') },
                    { text: 'Gateway IP', dataIndex: 'external_ip_address', width: 150 },
                    { text: 'SNAT', dataIndex: 'enable_snat', width: 80, renderer: booleanRenderer },
                    stateColumn(),
                ],
            }, {
                xtype: 'pvnResourceGrid', title: 'Interfaces', collection: 'router-interfaces',
                resourceLabel: 'Router interface', createFields: routerInterfaceFields,
                allowDelete: true,
                columns: [
                    { text: 'Router', dataIndex: 'router_id', flex: 1, renderer: referenceRenderer('routers', 'Unavailable router') },
                    { text: 'Subnet', dataIndex: 'subnet_id', flex: 1, renderer: referenceRenderer('subnets', 'Unavailable subnet', ['name', 'cidr']) },
                    { text: 'Port', dataIndex: 'port_id', flex: 1, renderer: referenceRenderer('ports', 'Managed logical port', ['name', 'mac_address']) },
                    stateColumn(),
                ],
            }],
        });

        Ext.define('PVN.panel.Ports', {
            extend: 'Ext.tab.Panel', alias: 'widget.pvnPorts', border: false,
            items: [{
                xtype: 'pvnResourceGrid', title: 'Ports', collection: 'ports', itemId: 'pvn-port-grid',
                resourceLabel: 'Port', createFields: portProvisionFields, createAction: 'provision',
                allowDelete: true, deleteAction: 'deprovision',
                columns: [
                    namedColumn('Port', 'Unnamed port', ['name', 'mac_address']),
                    { text: 'Network', dataIndex: 'network_id', flex: 1, renderer: referenceRenderer('networks', 'Unavailable network') },
                    { text: 'MAC address', dataIndex: 'mac_address', width: 155 },
                    { text: 'Fixed IPs', dataIndex: 'fixed_ips', flex: 1, renderer: listRenderer },
                    { text: 'Security groups', dataIndex: 'security_group_ids', flex: 1, renderer: multiReferenceRenderer('security-groups', 'Unavailable security group') },
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
            resourceLabel: 'Floating IP', createFields: floatingIPCreateFields,
            editFields: PVN.Resources['floating-ips'].editFields, allowDelete: true,
            columns: [
                namedColumn('Floating IP', 'Unallocated address', ['address', 'name']),
                { text: 'Provider', dataIndex: 'provider_network_id', flex: 1, renderer: referenceRenderer('provider-networks', 'Unavailable provider') },
                { text: 'Fixed IP', dataIndex: 'fixed_ip_address', width: 150 },
                { text: 'Port', dataIndex: 'port_id', flex: 1, renderer: referenceRenderer('ports', 'Unavailable port', ['name', 'mac_address']) },
                { text: 'Router', dataIndex: 'router_id', flex: 1, renderer: referenceRenderer('routers', 'Unavailable router') },
                { text: 'Status', dataIndex: 'status', width: 125, renderer: statusRenderer },
            ],
        });

        Ext.define('PVN.panel.SecurityGroups', {
            extend: 'Ext.tab.Panel', alias: 'widget.pvnSecurityGroups', border: false,
            items: [{
                xtype: 'pvnResourceGrid', title: 'Security Groups', collection: 'security-groups',
                resourceLabel: 'Security group', createFields: securityGroupCreateFields,
                editFields: securityGroupCreateFields, allowDelete: true,
                columns: [
                    namedColumn('Security group', 'Unnamed security group'),
                    { text: 'Description', dataIndex: 'description', flex: 2, renderer: html },
                    { text: 'Stateful', dataIndex: 'stateful', width: 90, renderer: booleanRenderer },
                    { text: 'Managed', dataIndex: 'managed', width: 90, renderer: booleanRenderer },
                    stateColumn(),
                ],
            }, {
                xtype: 'pvnResourceGrid', title: 'Rules', collection: 'security-group-rules',
                resourceLabel: 'Security group rule', createFields: securityRuleCreateFields,
                editFields: PVN.Resources['security-group-rules'].editFields, allowDelete: true,
                columns: [
                    { text: 'Security group', dataIndex: 'security_group_id', flex: 1, renderer: referenceRenderer('security-groups', 'Unavailable security group') },
                    { text: 'Direction', dataIndex: 'direction', width: 100 },
                    { text: 'Protocol', dataIndex: 'protocol', width: 100 },
                    { text: 'Ports', dataIndex: 'port_range_min', width: 110, renderer: function (value, _meta, record) { return value ? html(value + '-' + (record.get('port_range_max') || value)) : '-'; } },
                    { text: 'Remote CIDR', dataIndex: 'remote_cidr', width: 150 },
                    { text: 'Remote group', dataIndex: 'remote_group_id', flex: 1, renderer: referenceRenderer('security-groups', 'Unavailable security group') },
                    { text: 'Action', dataIndex: 'action', width: 90 },
                    stateColumn(),
                ],
            }],
        });

        Ext.define('PVN.panel.ProviderNetworks', {
            extend: 'Ext.tab.Panel', alias: 'widget.pvnProviderNetworks', border: false,
            items: [{
                xtype: 'pvnResourceGrid', title: 'Provider Networks', collection: 'provider-networks',
                resourceLabel: 'Provider network', createFields: providerNetworkCreateFields,
                editFields: providerNetworkEditFields, allowDelete: true,
                columns: [
                    namedColumn('Provider network', 'Unnamed provider network'),
                    { text: 'Default segment', dataIndex: 'default_segment_id', flex: 1, renderer: referenceRenderer('provider-segments', 'No default segment') },
                    { text: 'Description', dataIndex: 'description', flex: 2, renderer: html },
                    stateColumn(),
                ],
            }, {
                xtype: 'pvnResourceGrid', title: 'Segments', collection: 'provider-segments',
                resourceLabel: 'Provider segment', createFields: providerSegmentCreateFields,
                editFields: PVN.Resources['provider-segments'].editFields, allowDelete: true,
                columns: [
                    namedColumn('Segment', 'Unnamed segment'),
                    { text: 'Provider', dataIndex: 'provider_network_id', flex: 1, renderer: referenceRenderer('provider-networks', 'Unavailable provider') },
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
                { text: 'Target', dataIndex: 'target_id', flex: 1, renderer: operationTargetRenderer },
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
