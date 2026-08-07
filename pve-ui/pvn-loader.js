(function pvnLoader(window) {
    // Ext JS 7 resolves callParent() through Function.caller. Keeping the
    // class overrides below in strict mode makes that caller invisible and
    // aborts component construction before the PVN navigation tree mounts.

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
            var icon = /^(ok|healthy|up|active|ready|bound|complete|completed|success|connected)$/.test(normalized)
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
                    // Proxmox.RestProxy does not add the API2 prefix. Keep the
                    // collection request on the authenticated PVE dispatcher
                    // instead of falling through to pveproxy's file handler.
                    url: '/api2/json/pvn/' + collection,
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

        var writableFields = {
            networks: ['name', 'description', 'mtu', 'external', 'provider_network_id'],
            subnets: ['name', 'network_id', 'cidr', 'gateway_ip', 'enable_dhcp', 'allocation_pools'],
            routers: ['name', 'description', 'external_network_id', 'external_subnet_id', 'external_ip_address', 'enable_snat'],
            ports: ['name', 'network_id', 'subnet_id', 'fixed_ip_address', 'mac_address', 'security_group_ids'],
            'floating-ips': ['provider_network_id', 'address', 'fixed_ip_address', 'port_id', 'router_id'],
            'security-groups': ['name', 'description', 'stateful'],
            'security-group-rules': [
                'security_group_id', 'direction', 'ethertype', 'protocol', 'port_range_min',
                'port_range_max', 'remote_cidr', 'remote_group_id', 'action', 'description',
            ],
            'provider-networks': ['name', 'description', 'default_segment_id'],
            'provider-segments': ['name', 'provider_network_id', 'network_type', 'physical_network', 'vlan_id'],
        };

        function editableResource(resource, collection) {
            var data = dataOf(resource);
            var result = {};
            (writableFields[collection] || []).forEach(function (key) {
                if (data[key] !== undefined) {
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

        function formPayload(values, fields, resource, isCreate, collection) {
            var payload = isCreate ? {} : editableResource(resource, collection);
            fields.forEach(function (spec) {
                var value = normalizeFieldValue(spec, values[spec.name]);
                if (isCreate && !spec.required && !spec.includeEmpty &&
                    (value === '' || value === undefined || value === null || Array.isArray(value) && value.length === 0)) {
                    return;
                }
                if (!isCreate && spec.type === 'number' && value === '') {
                    value = 0;
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
            writableFields: writableFields,
        };

        Ext.define('PVN.model.Resource', {
            extend: 'Ext.data.Model',
            idProperty: 'id',
            fields: [
                'id', 'name', 'description', 'state', 'status', 'last_error',
                'revision', 'applied_revision', 'created_at', 'updated_at',
                'network_id', 'subnet_id', 'router_id', 'port_id',
                'security_group_id', 'security_group_ids', 'provider_network_id',
                'default_segment_id', 'node_id', 'cidr', 'gateway_ip', 'address',
                'allocation_pools', 'chassis_id', 'ovn_controller', 'gateway_priority',
                'external_network_id', 'external_subnet_id', 'external_ip_address',
                'fixed_ip_address', 'fixed_ips', 'mac_address', 'management_address',
                'physical_network', 'network_type', 'vlan_id', 'mtu', 'roles',
                'central_role', 'last_seen_at', 'action', 'kind', 'target_kind',
                'target_id', 'target_revision', 'started_at', 'completed_at', 'error',
                'binding_status', 'requested_chassis', 'lsp_name', 'nic', 'vmid',
                'generation', 'protocol', 'direction', 'ethertype', 'remote_cidr',
                'remote_group_id', 'port_range_min', 'port_range_max', 'read_only',
                'managed', 'enable_dhcp', 'enable_snat', 'external',
                'stateful', 'admin_state_up', 'enabled',
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
                var watching = true;
                return function () {
                    if (!watching) return;
                    watching = false;
                    var index = catalogWatchers.indexOf(callback);
                    if (index !== -1) catalogWatchers.splice(index, 1);
                };
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
                        var payload = formPayload(values, fields, resource, isCreate, me.collection);
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

        function requestPromise(url, method, params) {
            return new Promise(function (resolve, reject) {
                request({
                    url: url,
                    method: method || 'GET',
                    params: params || {},
                    success: function (response) {
                        resolve(response && response.result ? response.result.data : null);
                    },
                    failure: function (response) {
                        reject(new Error(apiError(response)));
                    },
                });
            });
        }

        function validNodeName(value) {
            return typeof value === 'string' && /^[A-Za-z0-9._-]+$/.test(value);
        }

        function validVMID(value) {
            return Number.isSafeInteger(Number(value)) && Number(value) > 0;
        }

        function validNIC(value) {
            return typeof value === 'string' && /^net(?:[0-9]|[12][0-9]|3[01])$/.test(value);
        }

        function validMAC(value) {
            return typeof value === 'string' && /^[A-Fa-f0-9]{2}(?::[A-Fa-f0-9]{2}){5}$/.test(value);
        }

        function digestOf(config) {
            var digest = config && config.digest;
            if (typeof digest !== 'string' || !/^[A-Fa-f0-9]{40,128}$/.test(digest)) {
                throw new Error('PVE returned a VM config without a valid digest');
            }
            return digest;
        }

        function revisionOf(port) {
            if (!Number.isSafeInteger(Number(port.revision)) || Number(port.revision) < 1) {
                throw new Error('PVN port has no valid revision');
            }
            return Number(port.revision);
        }

        function generationOf(port) {
            if (!Number.isSafeInteger(Number(port.generation)) || Number(port.generation) < 1) {
                throw new Error('PVN port has no valid generation');
            }
            return Number(port.generation);
        }

        function nicOptions(value) {
            if (typeof value !== 'string') return null;
            var parts = value.split(',');
            var first = parts.shift().split('=', 2);
            if (first.length !== 2) return null;
            var result = { model: first[0], mac: first[1] };
            parts.forEach(function (part) {
                var separator = part.indexOf('=');
                var key = separator > 0 ? part.slice(0, separator) : '';
                if (!key || Object.prototype.hasOwnProperty.call(result, key)) {
                    result.invalid = true;
                } else {
                    result[key] = part.slice(separator + 1);
                }
            });
            return result;
        }

        function isOwnedPVNNIC(value, macAddress, linkDown) {
            var options = nicOptions(value);
            if (!options || options.model !== 'virtio' || options.bridge !== 'br-int' || options.firewall !== '0') return false;
            if (String(options.mac).toLowerCase() !== String(macAddress).trim().toLowerCase()) return false;
            var allowed = { model: true, mac: true, bridge: true, firewall: true, link_down: true };
            if (Object.keys(options).some(function (key) { return !allowed[key]; })) return false;
            if (options.link_down !== undefined && options.link_down !== '0' && options.link_down !== '1') return false;
            if (linkDown === undefined) return true;
            return (options.link_down === '1') === linkDown;
        }

        function qemuPath(node, vmid, suffix) {
            if (!validNodeName(node) || !validVMID(vmid)) throw new Error('Select a valid PVE node and QEMU VM');
            return '/nodes/' + encodeURIComponent(node) + '/qemu/' + Number(vmid) + '/' + suffix;
        }

        var pveBridge = {
            getQemuConfig: function (node, vmid) {
                return requestPromise(qemuPath(node, vmid, 'config'), 'GET');
            },
            getQemuStatus: function (node, vmid) {
                return requestPromise(qemuPath(node, vmid, 'status/current'), 'GET');
            },
            listQemuVMs: function () {
                return requestPromise('/cluster/resources', 'GET', { type: 'vm' }).then(function (items) {
                    if (!Array.isArray(items)) return [];
                    return items.filter(function (item) {
                        return item && item.type === 'qemu' && validVMID(item.vmid) && validNodeName(item.node);
                    }).map(function (item) {
                        return {
                            vmid: Number(item.vmid),
                            node: item.node,
                            name: text(item.name),
                            display: (text(item.name) || 'Unnamed VM') + ' (VM ' + Number(item.vmid) + ') on ' + item.node,
                        };
                    });
                });
            },
            setQemuNIC: function (node, vmid, update) {
                if (!validNIC(update.nic) || !validMAC(update.macAddress)) throw new Error('Invalid PVN VM NIC identity');
                var params = { digest: update.digest };
                digestOf(params);
                params[update.nic] = 'virtio=' + update.macAddress + ',bridge=br-int,firewall=0,link_down=' + (update.linkDown ? '1' : '0');
                return requestPromise(qemuPath(node, vmid, 'config'), 'PUT', params);
            },
            deleteQemuNIC: function (node, vmid, digest, nic) {
                if (!validNIC(nic)) throw new Error('Invalid PVN VM NIC slot');
                digestOf({ digest: digest });
                return requestPromise(qemuPath(node, vmid, 'config'), 'PUT', { digest: digest, delete: nic });
            },
        };

        var managerAPI = {
            getPort: function (id) {
                return requestPromise(resourceURL('ports', id), 'GET');
            },
            attach: function (id, input, revision) {
                return requestPromise(resourceURL('ports', id, 'attach'), 'POST', writeParams(input, revision));
            },
            detach: function (id, input, revision) {
                return requestPromise(resourceURL('ports', id, 'detach'), 'POST', writeParams(input, revision));
            },
            resolve: function (node, vmid, nic) {
                return requestPromise('/pvn/runtime/ports/resolve', 'GET', {
                    node: node, vmid: Number(vmid), nic: nic,
                });
            },
        };

        function assertPortIdentity(port) {
            if (!port || !port.id || !validMAC(port.mac_address)) throw new Error('PVN port has no valid ID or MAC address');
            revisionOf(port);
            generationOf(port);
        }

        function assertTarget(target) {
            if (!target || !target.nodeID || !validNodeName(target.nodeName) ||
                !validVMID(target.vmid) || !validNIC(target.nic)) {
                throw new Error('Select a valid PVN node, QEMU VM, and netN slot');
            }
        }

        function lifecycleRuntime(options) {
            options = options || {};
            return {
                attempts: options.pollAttempts === undefined ? 90 : options.pollAttempts,
                interval: options.pollIntervalMs === undefined ? 500 : options.pollIntervalMs,
                sleep: options.sleep || function (delay) {
                    return new Promise(function (resolve) { window.setTimeout(resolve, delay); });
                },
                emit: options.onStep || function () {},
            };
        }

        async function assertRunning(bridge, target) {
            var status = await bridge.getQemuStatus(target.nodeName, target.vmid);
            if (!status || status.status !== 'running' && status.qmpstatus !== 'running') {
                throw new Error('PVN can attach or detach only a running QEMU VM');
            }
        }

        async function waitForPort(api, portID, expected, options) {
            var runtime = lifecycleRuntime(options);
            for (var attempt = 0; attempt < runtime.attempts; attempt += 1) {
                var port = await api.getPort(portID);
                if (port.binding_status === expected) {
                    if (port.state === 'ready' && Number(port.revision) > 0 &&
                        Number(port.applied_revision) === Number(port.revision)) return port;
                    await runtime.sleep(runtime.interval);
                    continue;
                }
                if (port.state === 'error' || port.binding_status === 'error') {
                    throw new Error(port.last_error || 'PVN port entered an error state');
                }
                var valid = expected === 'bound' ? port.binding_status === 'binding' : port.binding_status === 'detaching';
                if (!valid) throw new Error('PVN port changed unexpectedly to ' + (port.binding_status || 'unknown'));
                await runtime.sleep(runtime.interval);
            }
            throw new Error('Timed out waiting for PVN port to become ' + expected);
        }

        async function waitForNIC(bridge, target, predicate, options) {
            var runtime = lifecycleRuntime(options);
            for (var attempt = 0; attempt < runtime.attempts; attempt += 1) {
                var config = await bridge.getQemuConfig(target.nodeName, target.vmid);
                if (predicate(config[target.nic])) return config;
                await runtime.sleep(runtime.interval);
            }
            throw new Error('Timed out waiting for PVE ' + target.nic + ' configuration');
        }

        function matchesTarget(port, target, generation) {
            return port && port.node_id === target.nodeID && Number(port.vmid) === Number(target.vmid) &&
                port.nic === target.nic && Number(port.generation) === Number(generation);
        }

        function sameAttachment(port, target, generation) {
            return matchesTarget(port, target, generation) &&
                ['binding', 'bound', 'error'].indexOf(port.binding_status) !== -1;
        }

        function sameUnboundGeneration(port, generation) {
            return port && Number(port.generation) === Number(generation) && port.binding_status === 'unbound' &&
                !text(port.node_id) && !Number(port.vmid) && !text(port.nic);
        }

        async function removeOwnedNIC(bridge, port, target, options) {
            var config = await bridge.getQemuConfig(target.nodeName, target.vmid);
            if (config[target.nic] === undefined) return;
            if (!isOwnedPVNNIC(config[target.nic], port.mac_address)) {
                throw new Error('Refusing to delete ' + target.nic + ' because it is not the selected PVN port');
            }
            await bridge.deleteQemuNIC(target.nodeName, target.vmid, digestOf(config), target.nic);
            await waitForNIC(bridge, target, function (value) { return value === undefined; }, options);
        }

        async function rollbackAttach(api, bridge, port, target, managerAccepted, nicMayBeStaged, options) {
            var errors = [];
            var safeToDelete = nicMayBeStaged && !managerAccepted;
            if (managerAccepted) {
                try {
                    var current = await api.getPort(port.id);
                    if (current.binding_status === 'binding' || current.binding_status === 'bound' || current.binding_status === 'error') {
                        current = await api.detach(current.id, { generation: generationOf(current) }, revisionOf(current));
                    }
                    if (current.binding_status === 'detaching') current = await waitForPort(api, current.id, 'unbound', options);
                    safeToDelete = nicMayBeStaged && current.binding_status === 'unbound';
                } catch (error) {
                    errors.push(error instanceof Error ? error : new Error(String(error)));
                }
            }
            if (safeToDelete) {
                try {
                    await removeOwnedNIC(bridge, port, target, options);
                } catch (error) {
                    errors.push(error instanceof Error ? error : new Error(String(error)));
                }
            }
            return errors;
        }

        function lifecycleError(step, reason, rollbackErrors) {
            var message = reason instanceof Error ? reason.message : String(reason);
            if (rollbackErrors && rollbackErrors.length) {
                message += '. Cleanup also failed: ' + rollbackErrors.map(function (error) { return error.message; }).join('; ');
            }
            var error = new Error('VM port operation failed while ' + step.replace(/-/g, ' ') + ': ' + message);
            error.step = step;
            error.rollbackErrors = rollbackErrors || [];
            return error;
        }

        async function attachVMPort(api, bridge, initialPort, target, options) {
            options = options || {};
            var port = initialPort;
            var step = 'checking-vm';
            var managerAccepted = false;
            var managerStateAmbiguous = false;
            var nicMayBeStaged = false;
            function emit(next) { step = next; if (options.onStep) options.onStep(next); }
            try {
                assertPortIdentity(port);
                assertTarget(target);
                if (port.binding_status !== 'unbound') throw new Error('Only an unbound PVN port can be attached');
                emit('checking-vm');
                await assertRunning(bridge, target);
                var config = await bridge.getQemuConfig(target.nodeName, target.vmid);
                if (config[target.nic] !== undefined) throw new Error(target.nic + ' already exists on VM ' + target.vmid);

                emit('staging-nic');
                nicMayBeStaged = true;
                await bridge.setQemuNIC(target.nodeName, target.vmid, {
                    digest: digestOf(config), nic: target.nic, macAddress: port.mac_address, linkDown: true,
                });
                await waitForNIC(bridge, target, function (value) {
                    return isOwnedPVNNIC(value, port.mac_address, true);
                }, options);

                emit('requesting-binding');
                var nextGeneration = generationOf(port) + 1;
                try {
                    port = await api.attach(port.id, {
                        node_id: target.nodeID, vmid: Number(target.vmid), nic: target.nic,
                        generation: generationOf(port),
                    }, revisionOf(port));
                    managerAccepted = true;
                } catch (reason) {
                    var current;
                    try {
                        current = await api.getPort(port.id);
                    } catch (_error) {
                        managerStateAmbiguous = true;
                        throw reason;
                    }
                    if (!sameAttachment(current, target, nextGeneration)) {
                        managerStateAmbiguous = !sameUnboundGeneration(current, generationOf(port));
                        throw reason;
                    }
                    port = current;
                    managerAccepted = true;
                }

                emit('waiting-for-binding');
                if (port.binding_status !== 'bound') port = await waitForPort(api, port.id, 'bound', options);
                emit('enabling-nic');
                config = await bridge.getQemuConfig(target.nodeName, target.vmid);
                if (!isOwnedPVNNIC(config[target.nic], port.mac_address)) {
                    throw new Error(target.nic + ' no longer matches the PVN port');
                }
                await bridge.setQemuNIC(target.nodeName, target.vmid, {
                    digest: digestOf(config), nic: target.nic, macAddress: port.mac_address, linkDown: false,
                });
                await waitForNIC(bridge, target, function (value) {
                    return isOwnedPVNNIC(value, port.mac_address, false);
                }, options);
                return port;
            } catch (reason) {
                if (options.onStep) options.onStep('rolling-back');
                var rollbackErrors = managerStateAmbiguous
                    ? [new Error('PVN manager state is unknown; the staged NIC was left link-down')]
                    : await rollbackAttach(api, bridge, port, target, managerAccepted, nicMayBeStaged, options);
                throw lifecycleError(step, reason, rollbackErrors);
            }
        }

        async function detachVMPort(api, bridge, initialPort, target, options) {
            options = options || {};
            var port = initialPort;
            var step = 'checking-vm';
            var detachAccepted = false;
            var managerStateAmbiguous = false;
            var nicMayBeDisabled = false;
            function emit(next) { step = next; if (options.onStep) options.onStep(next); }
            try {
                assertPortIdentity(port);
                assertTarget(target);
                if (port.node_id !== target.nodeID || Number(port.vmid) !== Number(target.vmid) || port.nic !== target.nic) {
                    throw new Error('The selected VM target does not match this PVN port attachment');
                }
                if (['binding', 'bound', 'error'].indexOf(port.binding_status) === -1) {
                    throw new Error('Only an attached PVN port can be detached');
                }
                emit('checking-vm');
                await assertRunning(bridge, target);
                var config = await bridge.getQemuConfig(target.nodeName, target.vmid);
                if (!isOwnedPVNNIC(config[target.nic], port.mac_address)) {
                    throw new Error('Refusing to change ' + target.nic + ' because it is not the selected PVN port');
                }

                emit('disabling-nic');
                nicMayBeDisabled = true;
                await bridge.setQemuNIC(target.nodeName, target.vmid, {
                    digest: digestOf(config), nic: target.nic, macAddress: port.mac_address, linkDown: true,
                });
                await waitForNIC(bridge, target, function (value) {
                    return isOwnedPVNNIC(value, port.mac_address, true);
                }, options);

                emit('requesting-detach');
                var originalGeneration = generationOf(port);
                try {
                    port = await api.detach(port.id, { generation: originalGeneration }, revisionOf(port));
                    detachAccepted = true;
                } catch (reason) {
                    var current;
                    try {
                        current = await api.getPort(port.id);
                    } catch (_error) {
                        managerStateAmbiguous = true;
                        throw reason;
                    }
                    var accepted = Number(current.generation) === originalGeneration &&
                        (current.binding_status === 'unbound' ||
                            current.binding_status === 'detaching' && matchesTarget(current, target, originalGeneration));
                    if (!accepted) {
                        var unchanged = sameAttachment(current, target, originalGeneration) &&
                            ['binding', 'bound', 'error'].indexOf(current.binding_status) !== -1;
                        managerStateAmbiguous = !unchanged;
                        throw reason;
                    }
                    port = current;
                    detachAccepted = true;
                }

                emit('waiting-for-detach');
                if (port.binding_status !== 'unbound') port = await waitForPort(api, port.id, 'unbound', options);
                emit('deleting-nic');
                await removeOwnedNIC(bridge, port, target, options);
                return port;
            } catch (reason) {
                var rollbackErrors = [];
                if (managerStateAmbiguous) {
                    rollbackErrors.push(new Error('PVN manager state is unknown; the VM NIC was left link-down'));
                } else if (!detachAccepted && nicMayBeDisabled) {
                    if (options.onStep) options.onStep('rolling-back');
                    try {
                        var rollbackConfig = await bridge.getQemuConfig(target.nodeName, target.vmid);
                        if (isOwnedPVNNIC(rollbackConfig[target.nic], port.mac_address)) {
                            await bridge.setQemuNIC(target.nodeName, target.vmid, {
                                digest: digestOf(rollbackConfig), nic: target.nic,
                                macAddress: port.mac_address, linkDown: false,
                            });
                            await waitForNIC(bridge, target, function (value) {
                                return isOwnedPVNNIC(value, port.mac_address, false);
                            }, options);
                        }
                    } catch (rollbackReason) {
                        rollbackErrors.push(rollbackReason instanceof Error ? rollbackReason : new Error(String(rollbackReason)));
                    }
                }
                throw lifecycleError(step, reason, rollbackErrors);
            }
        }

        function firstFreeNIC(config) {
            for (var index = 0; index < 32; index += 1) {
                if (config['net' + index] === undefined) return 'net' + index;
            }
            return null;
        }

        PVN.PortLifecycle = {
            attach: attachVMPort,
            detach: detachVMPort,
            firstFreeNIC: firstFreeNIC,
            isOwnedPVNNIC: isOwnedPVNNIC,
            managerAPI: managerAPI,
            pveBridge: pveBridge,
        };

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
                var stopCatalogWatch = PVN.Catalog.watch(function () {
                    if (me.destroyed || me.destroying) return;
                    var view = me.getView && me.getView();
                    if (view && !view.destroyed && !view.destroying && view.ownerGrid && view.refresh) {
                        view.refresh();
                    }
                });
                me.listeners = Ext.apply({}, me.listeners || {}, {
                    activate: reload,
                    itemdblclick: function (_view, record) { openDetails(record); },
                    destroy: function () { stopCatalogWatch(); },
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

            overviewRows: function (health) {
                var capacity = health.capacity || {};
                var capacityDetails = [];
                var onlineNodes = Array.isArray(capacity.online_nodes) ? capacity.online_nodes : [];
                var missingNodes = Array.isArray(capacity.missing_nodes) ? capacity.missing_nodes : [];
                var staleNodes = Array.isArray(capacity.stale_nodes) ? capacity.stale_nodes : [];

                if (onlineNodes.length) {
                    capacityDetails.push(onlineNodes.length + ' online: ' + onlineNodes.join(', '));
                }
                if (missingNodes.length) {
                    capacityDetails.push('Missing: ' + missingNodes.join(', '));
                }
                if (staleNodes.length) {
                    capacityDetails.push('Stale: ' + staleNodes.join(', '));
                }
                if (text(capacity.reason)) {
                    capacityDetails.push(capacity.reason);
                }

                return [{
                    section: 'Cluster',
                    component: health.cluster || 'PVN cluster',
                    status: health.status || 'unknown',
                    details: 'PVN ' + (health.version || 'unknown'),
                }, {
                    section: 'Control plane',
                    component: 'Control database',
                    status: health.database,
                    details: 'Persistent PVN resource state',
                }, {
                    section: 'Control plane',
                    component: 'OVN Northbound',
                    status: health.ovn_northbound,
                    details: 'Logical network intent',
                }, {
                    section: 'Control plane',
                    component: 'OVN Southbound',
                    status: health.ovn_southbound,
                    details: 'Chassis and runtime state',
                }, {
                    section: 'Control plane',
                    component: 'Reconciler',
                    status: health.reconciler,
                    details: 'Desired-state reconciliation',
                }, {
                    section: 'Security',
                    component: 'Default security policy',
                    status: health.default_security_policy,
                    details: 'Managed baseline port policy',
                }, {
                    section: 'Capacity',
                    component: 'PVE node readiness',
                    status: capacity.ready === true ? 'ready' : capacity.ready === false ? 'degraded' : 'unknown',
                    details: capacityDetails.join(' \u00b7 ') || 'All required PVE nodes are available',
                }];
            },

            loadOverview: function () {
                var me = this;
                var grid = me.down('#pvn-overview-grid');
                var store = grid && grid.getStore ? grid.getStore() : me.overviewStore;
                var meta = me.down('#pvn-overview-meta');
                if (me.setLoading) {
                    me.setLoading('Loading PVN status...');
                }
                request({
                    url: '/pvn/health',
                    method: 'GET',
                    success: function (response) {
                        var health = response && response.result && response.result.data || {};
                        if (store && store.loadData) {
                            store.loadData(me.overviewRows(health));
                        }
                        if (meta && meta.setText) {
                            meta.setText(
                                '<b>' + html(health.cluster || 'PVN') + '</b>' +
                                ' &middot; Version ' + html(health.version || 'unknown') +
                                (health.time ? ' &middot; Checked ' + html(health.time) : '')
                            );
                        }
                        if (me.setLoading) {
                            me.setLoading(false);
                        }
                    },
                    failure: function (response) {
                        if (store && store.loadData) {
                            store.loadData([{
                                section: 'Manager', component: 'PVN API', status: 'unavailable',
                                details: apiError(response),
                            }]);
                        }
                        if (meta && meta.setText) {
                            meta.setText('<b>PVN status unavailable</b>');
                        }
                        if (me.setLoading) {
                            me.setLoading(false);
                        }
                    },
                });
            },

            initComponent: function () {
                var me = this;
                me.overviewStore = Ext.create('Ext.data.Store', {
                    fields: ['section', 'component', 'status', 'details'],
                    data: [],
                });
                Ext.apply(me, {
                    layout: 'fit',
                    tbar: [{
                        text: 'Refresh',
                        iconCls: 'fa fa-refresh',
                        handler: function () { me.loadOverview(); },
                    }, '->', {
                        xtype: 'tbtext',
                        itemId: 'pvn-overview-meta',
                        text: 'PVN status has not been loaded',
                    }],
                    items: [{
                        xtype: 'grid',
                        itemId: 'pvn-overview-grid',
                        border: false,
                        store: me.overviewStore,
                        columns: [{
                            text: 'Area', dataIndex: 'section', width: 145,
                        }, {
                            text: 'Component', dataIndex: 'component', minWidth: 190, flex: 1,
                            renderer: function (value) { return '<b>' + html(value) + '</b>'; },
                        }, {
                            text: 'Status', dataIndex: 'status', width: 135, renderer: statusRenderer,
                        }, {
                            text: 'Details', dataIndex: 'details', minWidth: 260, flex: 2, renderer: html,
                        }],
                        viewConfig: {
                            deferEmptyText: false,
                            emptyText: '<div class="x-grid-empty">No PVN status available.</div>',
                            stripeRows: true,
                        },
                    }],
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

        Ext.define('PVN.model.QemuVM', {
            extend: 'Ext.data.Model',
            idProperty: 'vmid',
            fields: ['vmid', 'name', 'node', 'display'],
        });

        Ext.define('PVN.form.QemuCombo', {
            extend: 'Ext.form.field.ComboBox',
            alias: 'widget.pvnQemuCombo',
            queryMode: 'local',
            valueField: 'vmid',
            displayField: 'display',
            forceSelection: true,
            editable: true,

            setNodeName: function (nodeName) {
                var me = this;
                me.nodeName = nodeName || '';
                me.store.clearFilter();
                if (me.nodeName) {
                    me.store.filterBy(function (record) { return record.get('node') === me.nodeName; });
                }
            },

            loadVMs: function () {
                var me = this;
                return pveBridge.listQemuVMs().then(function (items) {
                    me.store.loadData(items);
                    me.setNodeName(me.nodeName);
                    return items;
                }).catch(function (error) {
                    Ext.Msg.alert('VM inventory unavailable', html(error.message));
                    return [];
                });
            },

            initComponent: function () {
                var me = this;
                me.store = Ext.create('Ext.data.Store', { model: 'PVN.model.QemuVM', data: [] });
                me.listConfig = {
                    emptyText: 'No running or stopped QEMU VMs on this node',
                    getInnerTpl: function () { return '<b>{name:htmlEncode}</b> (VM {vmid})'; },
                };
                me.callParent();
            },
        });

        var lifecycleStepLabels = {
            'checking-vm': 'Checking VM state',
            'staging-nic': 'Creating a fail-closed NIC',
            'requesting-binding': 'Requesting OVN binding',
            'waiting-for-binding': 'Waiting for OVN binding',
            'enabling-nic': 'Enabling the VM NIC',
            'disabling-nic': 'Disabling the VM NIC',
            'requesting-detach': 'Requesting detach',
            'waiting-for-detach': 'Waiting for OVN cleanup',
            'deleting-nic': 'Removing the VM NIC',
            'rolling-back': 'Rolling back safely',
        };

        Ext.define('PVN.panel.PortAttachments', {
            extend: 'Ext.panel.Panel',
            alias: 'widget.pvnPortAttachments',
            border: false,
            bodyPadding: 16,
            scrollable: true,

            initComponent: function () {
                var me = this;
                var portStore = createStore('ports');
                var nodeStore = createStore('nodes');
                var busy = false;
                var inspectedConfig = null;

                var portField = Ext.create('Ext.form.field.ComboBox', {
                    fieldLabel: 'PVN port', store: portStore, queryMode: 'local',
                    valueField: 'id', displayField: 'pvn_label', forceSelection: true,
                    editable: true, width: 430,
                });
                var nodeField = Ext.create('Ext.form.field.ComboBox', {
                    fieldLabel: 'PVE node', store: nodeStore, queryMode: 'local',
                    valueField: 'id', displayField: 'pvn_label', forceSelection: true,
                    editable: true, width: 380,
                });
                var vmField = Ext.create('PVN.form.QemuCombo', {
                    fieldLabel: 'QEMU VM', width: 430,
                });
                var nicField = Ext.create('Ext.form.field.Text', {
                    fieldLabel: 'NIC slot', value: 'net0', width: 250,
                    regex: /^net(?:[0-9]|[12][0-9]|3[01])$/,
                    regexText: 'Use a net0 through net31 slot',
                });
                var progress = Ext.create('Ext.Component', {
                    hidden: true,
                    margin: '12 0 0 0',
                });
                var evidence = Ext.create('Ext.Component', {
                    margin: '16 0 0 0',
                    html: '<p>Select a port, node, and VM, then inspect the configuration.</p>',
                });
                var inspectButton;
                var attachButton;
                var detachButton;

                function recordByID(store, id) {
                    return id && store.getById ? store.getById(id) : null;
                }

                function selectedPort() {
                    return dataOf(recordByID(portStore, portField.getValue()));
                }

                function selectedNode() {
                    return dataOf(recordByID(nodeStore, nodeField.getValue()));
                }

                function selectedTarget() {
                    var node = selectedNode();
                    var vmid = Number(vmField.getValue());
                    var nic = text(nicField.getValue());
                    var target = { nodeID: node.id, nodeName: node.name, vmid: vmid, nic: nic };
                    assertTarget(target);
                    return target;
                }

                function setProgress(step) {
                    busy = Boolean(step);
                    if (step) {
                        progress.setHidden(false);
                        progress.update('<p><i class="fa fa-spinner fa-pulse"></i> <b>' +
                            html(lifecycleStepLabels[step] || step) + '</b><br>The VM NIC remains fail-closed during this step.</p>');
                    } else {
                        progress.setHidden(true);
                    }
                    updateControls();
                }

                function updateControls() {
                    var port = selectedPort();
                    var attached = port.id && port.binding_status !== 'unbound';
                    var node = selectedNode();
                    var actionable = Boolean(port.id && node.id && validVMID(vmField.getValue()) &&
                        validNIC(text(nicField.getValue())) && !busy);
                    nodeField.setDisabled(Boolean(attached || busy));
                    vmField.setDisabled(Boolean(attached || busy));
                    nicField.setDisabled(Boolean(attached || busy));
                    portField.setDisabled(busy);
                    inspectButton.setDisabled(!actionable);
                    attachButton.setHidden(Boolean(attached));
                    attachButton.setDisabled(!actionable || port.admin_state_up === false || node.enabled === false);
                    detachButton.setHidden(!attached);
                    detachButton.setDisabled(!actionable || ['binding', 'bound', 'error'].indexOf(port.binding_status) === -1);
                }

                function syncAttachment() {
                    var port = selectedPort();
                    inspectedConfig = null;
                    if (port.id && port.binding_status !== 'unbound') {
                        nodeField.setValue(port.node_id || '');
                        vmField.setValue(port.vmid || '');
                        nicField.setValue(port.nic || 'net0');
                    }
                    evidence.update('<p>' + html(port.id
                        ? humanName(port, 'Unnamed port', ['name', 'mac_address']) + ' is ' + (port.binding_status || 'unknown') + '.'
                        : 'No PVN ports are available.') + '</p>');
                    updateControls();
                }

                function syncNode() {
                    var node = selectedNode();
                    vmField.setNodeName(node.name || '');
                    vmField.setValue('');
                    updateControls();
                }

                async function inspect() {
                    var port = selectedPort();
                    var target;
                    try {
                        assertPortIdentity(port);
                        target = selectedTarget();
                        setProgress('checking-vm');
                        var results = await Promise.all([
                            pveBridge.getQemuConfig(target.nodeName, target.vmid),
                            pveBridge.getQemuStatus(target.nodeName, target.vmid),
                        ]);
                        inspectedConfig = results[0];
                        var status = results[1] || {};
                        if (port.binding_status === 'unbound') {
                            var available = firstFreeNIC(inspectedConfig);
                            if (available) nicField.setValue(available);
                        }
                        var runtime = null;
                        if (port.binding_status !== 'unbound') {
                            try {
                                runtime = await managerAPI.resolve(target.nodeName, target.vmid, target.nic);
                            } catch (_error) {
                                runtime = { status: 'unavailable' };
                            }
                        }
                        var nics = Object.keys(inspectedConfig).filter(function (key) { return /^net[0-9]+$/.test(key); }).sort();
                        var rows = nics.length ? nics.map(function (key) {
                            return '<tr><td><b>' + html(key) + '</b></td><td>' + html(inspectedConfig[key]) + '</td></tr>';
                        }).join('') : '<tr><td colspan="2">No configured NICs</td></tr>';
                        var owned = isOwnedPVNNIC(inspectedConfig[target.nic], port.mac_address);
                        evidence.update(
                            '<h3 style="margin-top:0">' + html(humanName(port, 'Unnamed port', ['name', 'mac_address'])) + '</h3>' +
                            '<p>VM status: <b>' + html(status.status || status.qmpstatus || 'unknown') + '</b> &middot; ' +
                            html(target.nic) + ': <b>' + html(owned ? 'matches selected PVN port' : inspectedConfig[target.nic] === undefined ? 'free' : 'owned by another NIC') + '</b>' +
                            (runtime ? ' &middot; runtime: <b>' + html(runtime.status || 'matched') + '</b>' : '') + '</p>' +
                            '<table class="x-grid-item" style="width:100%"><tbody>' + rows + '</tbody></table>'
                        );
                    } catch (error) {
                        Ext.Msg.alert('VM inspection failed', html(error.message));
                    } finally {
                        setProgress(null);
                    }
                }

                async function runLifecycle(action) {
                    var port = selectedPort();
                    try {
                        var target = selectedTarget();
                        var result = action === 'attach'
                            ? await attachVMPort(managerAPI, pveBridge, port, target, { onStep: setProgress })
                            : await detachVMPort(managerAPI, pveBridge, port, target, { onStep: setProgress });
                        evidence.update('<p><i class="fa fa-check good"></i> <b>' +
                            html(humanName(result, 'Port', ['name', 'mac_address'])) + '</b> is now ' +
                            html(result.binding_status) + '.</p>');
                        refreshStore(portStore);
                    } catch (error) {
                        Ext.Msg.alert('VM port operation failed', html(error.message));
                        refreshStore(portStore);
                    } finally {
                        setProgress(null);
                    }
                }

                inspectButton = Ext.create('Ext.button.Button', {
                    text: 'Inspect', iconCls: 'fa fa-search', disabled: true, handler: inspect,
                });
                attachButton = Ext.create('Ext.button.Button', {
                    text: 'Attach', iconCls: 'fa fa-plug', disabled: true,
                    handler: function () { runLifecycle('attach'); },
                });
                detachButton = Ext.create('Ext.button.Button', {
                    text: 'Detach', iconCls: 'fa fa-unlink', disabled: true, hidden: true,
                    handler: function () { runLifecycle('detach'); },
                });

                if (portField.on) portField.on('change', syncAttachment);
                if (nodeField.on) nodeField.on('change', syncNode);
                if (vmField.on) vmField.on('change', updateControls);
                if (nicField.on) nicField.on('change', updateControls);
                if (portStore.on) portStore.on('load', function () {
                    if (!recordByID(portStore, portField.getValue())) {
                        var first = portStore.getAt ? portStore.getAt(0) : null;
                        portField.setValue(first ? first.get('id') : '');
                    }
                    syncAttachment();
                });
                if (nodeStore.on) nodeStore.on('load', function () {
                    var port = selectedPort();
                    var node = selectedNode();
                    vmField.setNodeName(node.name || '');
                    if (port.id && port.binding_status !== 'unbound') {
                        vmField.setValue(port.vmid || '');
                    }
                    updateControls();
                });

                Ext.apply(me, {
                    items: [{
                        xtype: 'form', border: false, bodyStyle: 'background:transparent',
                        fieldDefaults: { labelWidth: 100 },
                        items: [portField, nodeField, vmField, nicField],
                        buttons: [inspectButton, attachButton, detachButton],
                    }, progress, evidence],
                    tbar: [{
                        text: 'Refresh', iconCls: 'fa fa-refresh', handler: function () {
                            refreshStore(portStore);
                            refreshStore(nodeStore);
                            vmField.loadVMs();
                        },
                    }],
                    listeners: {
                        activate: function () {
                            refreshStore(portStore);
                            refreshStore(nodeStore);
                            vmField.loadVMs();
                        },
                    },
                });
                me.callParent();
            },
        });

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
                xtype: 'pvnPortAttachments', title: 'VM Attachments', itemId: 'pvn-port-attachments',
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
