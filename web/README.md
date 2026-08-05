# PVN web manager

The PVN manager UI is a self-contained React/Vite application. The production
bundle loads no remote scripts, fonts, or stylesheets.

```sh
npm install
npm test
npm run build
```

The manager should serve `dist/` over HTTPS on port 8443, route unknown browser
paths back to `index.html`, and expose the control API at `/api/v1`. Before the
application renders it calls `GET /api/v1/session`; the response must contain
the authenticated PVE user and PVN CSRF token.

When opened by `pvn-loader.js`, the query string carries a one-time bridge
nonce and the PVE origin. The UI checks both against the iframe referrer before
enabling QEMU configuration reads or writes.
