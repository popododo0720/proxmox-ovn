# Proxmox UI adapter

`pvn-loader.js` adds a PVN item to the Datacenter configuration panel and
embeds the local manager at `https://<node>:8443`. It does not implement a
second login. The manager receives the existing PVE cookie and validates it
through the local PVE API.

The iframe bridge exposes only `GET` and narrowly constrained `PUT` requests
for `/nodes/<node>/qemu/<vmid>/config`. A PUT must carry a PVE digest plus one
PVN `netN` value, or `delete=netN`; unrelated VM settings are rejected. Every
message must match the generated nonce, the manager origin, and the exact
iframe window.

Install or remove the template marker with:

```sh
./inject.sh install
./inject.sh remove
```

Package scripts should run the patch again after a supported `pve-manager`
9.x upgrade. The injector patches only the known PVE 9 template signature,
directly after `pvemanagerlib.js`. An unknown signature or a missing Ext/PVE
class leaves the normal Proxmox UI untouched.
