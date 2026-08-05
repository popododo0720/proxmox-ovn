# PVN web manager

The PVN manager UI is a self-contained React/Vite application. The production
bundle loads no remote scripts, fonts, or stylesheets.

```sh
npm install
npm test
npm run build
```

The manager should serve `dist/` over HTTPS on port 8443 and expose the control
API at `/api/v1`. Views use hash routes so a basic static file handler is
sufficient. Before the application renders it calls `GET /api/v1/session`;
the response must contain the authenticated PVE user and PVN CSRF token.
Mutating API calls send that token in the `X-PVN-CSRF-Token` header and also
send an idempotency key; updates and deletes carry the resource revision in a
quoted `If-Match` header.

When opened by `pvn-loader.js`, the query string carries a one-time bridge
nonce and the PVE origin. The UI checks both against the iframe referrer before
enabling QEMU configuration reads or writes.
