# Proxmox UI adapter

`pvn-loader.js` adds a PVN item to the Datacenter configuration panel and
eight grouped child panels. The screens are native ExtJS components mounted
inside Proxmox; there is no iframe, second browser origin, or second login.

PVN resource requests use the authenticated Proxmox origin at
`/api2/json/pvn`. The PVE API extension checks the existing PVE ticket, CSRF,
and RBAC context before forwarding a credential-free request through the
local `/run/pvn-api/manager.sock`. VM NIC writes continue through the normal
PVE API and are limited to a digest-bound PVN `netN` add, link-state change,
or delete.

Install or remove the template marker with:

```sh
./inject.sh install
./inject.sh remove
```

Package scripts run the patch again after a supported `pve-manager` 9.x
upgrade. The injector patches only the known PVE 9 template signature,
directly after `pvemanagerlib.js`. An unknown signature or a missing Ext/PVE
class leaves the normal Proxmox UI untouched.
