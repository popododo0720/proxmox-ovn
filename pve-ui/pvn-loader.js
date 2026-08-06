(function pvnLoader(window, document) {
    'use strict';

    if (window.__pvnLoaderInstalled === true) {
        return;
    }
    window.__pvnLoaderInstalled = true;

    var SOURCE_UI = 'pvn-ui';
    var SOURCE_LOADER = 'pvn-loader';
    var REQUEST_TYPE = 'pvn:pve:request';
    var RESPONSE_TYPE = 'pvn:pve:response';
    var VERSION = 1;
    var MAX_WAIT_ATTEMPTS = 120;
    var waitAttempts = 0;

    function managerOrigin() {
        var hostname = window.location.hostname;
        var alreadyBracketed = hostname.charAt(0) === '[' && hostname.charAt(hostname.length - 1) === ']';
        var host = hostname.indexOf(':') === -1 || alreadyBracketed ? hostname : '[' + hostname + ']';
        return new URL('https://' + host + ':8443').origin;
    }

    function openCertificatePage() {
        var url = new URL('/', managerOrigin()).toString();
        var popup = window.open(url, '_blank', 'noopener,noreferrer');
        if (popup) {
            popup.opener = null;
        }
    }

    function randomNonce() {
        if (!window.crypto || typeof window.crypto.getRandomValues !== 'function') {
            throw new Error('PVN requires a cryptographically secure browser context');
        }
        var values = new Uint32Array(8);
        window.crypto.getRandomValues(values);
        return Array.prototype.map.call(values, function (value) {
            return value.toString(16).padStart(8, '0');
        }).join('');
    }

    function isPlainObject(value) {
        if (!value || Object.prototype.toString.call(value) !== '[object Object]') {
            return false;
        }
        var prototype = Object.getPrototypeOf(value);
        return prototype === Object.prototype || prototype === null;
    }

    function safeValue(value, depth) {
        if (value === null || typeof value === 'string' || typeof value === 'boolean') {
            return value;
        }
        if (typeof value === 'number' && Number.isFinite(value)) {
            return value;
        }
        if (depth >= 4) {
            throw new Error('PVE request parameters are too deeply nested');
        }
        if (Array.isArray(value)) {
            if (value.length > 64) {
                throw new Error('PVE request contains too many parameter values');
            }
            return value.map(function (item) { return safeValue(item, depth + 1); });
        }
        if (!isPlainObject(value)) {
            throw new Error('PVE request contains an unsupported parameter value');
        }
        var keys = Object.keys(value);
        if (keys.length > 64) {
            throw new Error('PVE request contains too many parameters');
        }
        var copy = Object.create(null);
        keys.forEach(function (key) {
            if (key === '__proto__' || key === 'prototype' || key === 'constructor') {
                throw new Error('PVE request contains an unsafe parameter name');
            }
            copy[key] = safeValue(value[key], depth + 1);
        });
        return copy;
    }

    function serializable(value) {
        if (value === undefined) {
            return null;
        }
        try {
            return JSON.parse(JSON.stringify(value));
        } catch (_error) {
            return null;
        }
    }

    function errorDetails(response) {
        var result = response && response.result;
        var raw = result && (result.message || result.error) || response && (response.htmlStatus || response.statusText) || 'PVE request failed';
        var message = typeof raw === 'string' ? raw : 'PVE request failed';
        message = message.replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim().slice(0, 300);
        var status = response && Number(response.status);
        return {
            message: message || 'PVE request failed',
            status: Number.isFinite(status) ? status : undefined,
        };
    }

    function validMessageShape(message) {
        if (!isPlainObject(message)) {
            return false;
        }
        var allowed = {
            source: true,
            type: true,
            version: true,
            nonce: true,
            id: true,
            method: true,
            path: true,
            params: true,
        };
        return Object.keys(message).every(function (key) { return allowed[key] === true; });
    }

    function requestParams(message) {
        if (message.method === 'GET') {
            if (message.params === undefined) {
                return {};
            }
            if (!isPlainObject(message.params) || Object.keys(message.params).length !== 0) {
                throw new Error('GET QEMU config requests must not contain parameters');
            }
            return {};
        }

        if (!isPlainObject(message.params)) {
            throw new Error('PUT QEMU config requests require parameters');
        }
        var keys = Object.keys(message.params);
        if (keys.length !== 2 || !Object.prototype.hasOwnProperty.call(message.params, 'digest')) {
            throw new Error('PUT QEMU config requests require digest and one NIC action');
        }
        var digest = message.params.digest;
        if (typeof digest !== 'string' || !/^[A-Fa-f0-9]{40,128}$/.test(digest)) {
            throw new Error('PUT QEMU config requests require a valid digest');
        }
        var actionKeys = keys.filter(function (key) { return key !== 'digest'; });
        var action = actionKeys[0];
        if (action === 'delete') {
            if (typeof message.params.delete !== 'string' || !/^net[0-9]+$/.test(message.params.delete)) {
                throw new Error('PUT delete must name exactly one netN device');
            }
        } else {
            if (!/^net[0-9]+$/.test(action) ||
                typeof message.params[action] !== 'string' ||
                !/^virtio=[A-Fa-f0-9]{2}(?::[A-Fa-f0-9]{2}){5},bridge=br-int,firewall=0,link_down=[01]$/.test(message.params[action])) {
                throw new Error('PUT may configure exactly one PVN virtio netN device');
            }
        }
        return safeValue(message.params, 0);
    }

    function createBridge(frame, nonce, origin) {
        function respond(id, ok, data, error) {
            if (!frame.contentWindow) {
                return;
            }
            frame.contentWindow.postMessage({
                source: SOURCE_LOADER,
                type: RESPONSE_TYPE,
                version: VERSION,
                nonce: nonce,
                id: id,
                ok: ok,
                data: ok ? serializable(data) : undefined,
                error: ok ? undefined : error,
            }, origin);
        }

        function onMessage(event) {
            if (event.origin !== origin || event.source !== frame.contentWindow) {
                return;
            }
            var message = event.data;
            var configPath = message && typeof message.path === 'string' &&
                /^\/nodes\/[A-Za-z0-9._-]+\/qemu\/[1-9][0-9]*\/config$/.test(message.path);
            var statusPath = message && typeof message.path === 'string' &&
                /^\/nodes\/[A-Za-z0-9._-]+\/qemu\/[1-9][0-9]*\/status\/current$/.test(message.path);
            if (!validMessageShape(message) ||
                message.source !== SOURCE_UI ||
                message.type !== REQUEST_TYPE ||
                message.version !== VERSION ||
                message.nonce !== nonce ||
                typeof message.id !== 'string' ||
                !/^[A-Za-z0-9_-]{1,128}$/.test(message.id) ||
                (message.method !== 'GET' && message.method !== 'PUT') ||
                (!configPath && !statusPath) ||
                (statusPath && message.method !== 'GET')) {
                return;
            }
            if (!window.Proxmox || !window.Proxmox.Utils || typeof window.Proxmox.Utils.API2Request !== 'function') {
                respond(message.id, false, null, { message: 'Proxmox API helper is unavailable' });
                return;
            }
            var params;
            try {
                params = requestParams(message);
            } catch (error) {
                respond(message.id, false, null, { message: error.message });
                return;
            }
            window.Proxmox.Utils.API2Request({
                url: message.path,
                method: message.method,
                params: params,
                success: function (response) {
                    var result = response && response.result;
                    respond(message.id, true, result ? result.data : null);
                },
                failure: function (response) {
                    respond(message.id, false, null, errorDetails(response));
                },
            });
        }

        window.addEventListener('message', onMessage, false);
        return function removeBridge() {
            window.removeEventListener('message', onMessage, false);
        };
    }

    function iframePanel() {
        var frame;
        var frameURL;
        return {
            xtype: 'panel',
            itemId: 'pvn',
            title: 'PVN',
            iconCls: 'fa fa-sitemap',
            layout: 'fit',
            border: false,
            dockedItems: [{
                xtype: 'toolbar',
                dock: 'top',
                items: [{
                    xtype: 'tbtext',
                    text: "First use: trust this node's PVN certificate before the embedded console can load.",
                }, '->', {
                    xtype: 'button',
                    itemId: 'pvn-trust-certificate',
                    text: 'Trust local PVN certificate',
                    iconCls: 'fa fa-external-link',
                    tooltip: 'Opens this node\'s PVN manager in a protected new tab. Review and accept the certificate warning, then return and reload PVN.',
                    handler: openCertificatePage,
                }, {
                    xtype: 'button',
                    itemId: 'pvn-reload',
                    text: 'Reload PVN',
                    iconCls: 'fa fa-refresh',
                    tooltip: 'Reloads the embedded PVN console after the certificate is trusted.',
                    handler: function () {
                        if (frame && frameURL) {
                            frame.src = frameURL;
                        }
                    },
                }],
            }],
            listeners: {
                afterrender: function (panel) {
                    var origin = managerOrigin();
                    var nonce;
                    try {
                        nonce = randomNonce();
                    } catch (error) {
                        panel.update('<div style="padding:24px;color:#b94a48">' + String(error.message) + '</div>');
                        return;
                    }
                    var url = new URL('/', origin);
                    url.searchParams.set('pveBridgeNonce', nonce);
                    url.searchParams.set('pveOrigin', window.location.origin);
                    frameURL = url.toString();
                    frame = document.createElement('iframe');
                    frame.title = 'PVN Network Manager';
                    frame.src = frameURL;
                    frame.referrerPolicy = 'strict-origin';
                    frame.setAttribute('sandbox', 'allow-scripts allow-forms allow-downloads allow-same-origin allow-top-navigation-by-user-activation');
                    frame.style.cssText = 'display:block;width:100%;height:100%;min-height:500px;border:0;background:#0d1319';
                    panel.body.dom.appendChild(frame);
                    var removeBridge = createBridge(frame, nonce, origin);
                    panel.on('beforedestroy', removeBridge, null, { single: true });
                },
            },
        };
    }

    function hasPVNItem(items) {
        if (!Array.isArray(items)) {
            return false;
        }
        return items.some(function (item) { return item && item.itemId === 'pvn'; });
    }

    function installMenu() {
        var Ext = window.Ext;
        var PVE = window.PVE;
        if (!Ext || !PVE || !Ext.ClassManager || typeof Ext.override !== 'function') {
            return false;
        }
        var baseClass = Ext.ClassManager.get('PVE.panel.Config') || PVE.panel && PVE.panel.Config;
        var dcClass = Ext.ClassManager.get('PVE.dc.Config') || PVE.dc && PVE.dc.Config;
        if (!baseClass || !dcClass || !baseClass.prototype || typeof baseClass.prototype.initComponent !== 'function') {
            return false;
        }
        if (baseClass.prototype.__pvnMenuPatched === true) {
            return true;
        }
        var original = baseClass.prototype.initComponent;
        Ext.override(baseClass, {
            initComponent: function () {
                var isDatacenter = this instanceof dcClass || this.$className === 'PVE.dc.Config';
                if (isDatacenter && Array.isArray(this.items) && !hasPVNItem(this.items)) {
                    this.items.push(iframePanel());
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
}(window, document));
