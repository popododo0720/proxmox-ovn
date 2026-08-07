import { useEffect, useState, type PropsWithChildren } from 'react';
import { ApiError } from './api/client';
import { useApi } from './api/context';
import type { SessionInfo } from './api/types';
import { ErrorState } from './components/ErrorState';
import { LoadingState } from './components/LoadingState';
import { OverviewPage } from './pages/OverviewPage';
import {
  FloatingIPsPage,
  NetworksPage,
  NodesPage,
  OperationsPage,
  PortsPage,
  ProjectsPage,
  ProviderNetworksPage,
  RoutersPage,
  SecurityGroupsPage,
} from './pages/ResourcePages';
import { pveBridge } from './pve/bridge';

const navigation = [
  { to: '/', label: 'Overview', glyph: '◫', end: true },
  { to: '/projects', label: 'Projects', glyph: 'P' },
  { to: '/networks', label: 'Networks & subnets', glyph: 'N' },
  { to: '/routers', label: 'Routers', glyph: 'R' },
  { to: '/ports', label: 'Ports & attachments', glyph: 'A' },
  { to: '/floating-ips', label: 'Floating IPs', glyph: 'F' },
  { to: '/security-groups', label: 'Security groups', glyph: 'S' },
  { to: '/provider-networks', label: 'Provider networks', glyph: 'E' },
  { to: '/nodes', label: 'Nodes & central', glyph: 'C' },
  { to: '/operations', label: 'Operations', glyph: 'O' },
];

export default function App() {
  return (
    <SessionGate>
      {(session) => <AppShell session={session} />}
    </SessionGate>
  );
}

function SessionGate({ children }: { children: (session: SessionInfo) => React.ReactNode }) {
  const api = useApi();
  const [session, setSession] = useState<SessionInfo | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let current = true;
    setSession(null);
    setError(null);
    void api.bootstrap()
      .then((value) => { if (current) setSession(value); })
      .catch((reason: unknown) => { if (current) setError(reason); });
    return () => { current = false; };
  }, [api, attempt]);

  if (session) return <>{children(session)}</>;
  if (!error) {
    return <main className="session-screen"><div className="session-panel"><Brand /><LoadingState label="Verifying your Proxmox session" /></div></main>;
  }

  const authError = error instanceof ApiError && error.status === 401;
  const permissionError = error instanceof ApiError && error.status === 403;
  const message = error instanceof Error ? error.message : 'PVN could not verify this PVE session';
  return (
    <main className="session-screen">
      <div className="session-panel">
        <Brand />
        <ErrorState
          title={authError ? 'Proxmox login required' : permissionError ? 'Permission denied' : 'PVN manager unavailable'}
          message={authError
            ? 'Sign in to Proxmox, then open PVN again from the Datacenter menu.'
            : permissionError
              ? 'Your PVE account does not have permission to audit PVN resources.'
              : message}
          onRetry={() => setAttempt((value) => value + 1)}
        />
        {authError && pveBridge.parentOrigin && <a className="button button-primary login-link" href={pveBridge.parentOrigin} target="_top">Open Proxmox login</a>}
      </div>
    </main>
  );
}

function AppShell({ session }: { session: SessionInfo }) {
  const embedded = new URLSearchParams(window.location.search).get('embedded') === '1';
  const [menuOpen, setMenuOpen] = useState(false);
  const readRoute = () => {
    const params = new URLSearchParams(window.location.search);
    const requestedRoute = window.location.hash.replace(/^#/, '')
      || (params.get('embedded') === '1' ? params.get('route') || '' : '');
    const rootedRoute = requestedRoute.startsWith('/') ? requestedRoute : `/${requestedRoute}`;
    return rootedRoute.replace(/\/+$/, '') || '/';
  };
  const [route, setRoute] = useState(readRoute);

  useEffect(() => {
    const update = () => setRoute(readRoute());
    window.addEventListener('hashchange', update);
    return () => window.removeEventListener('hashchange', update);
  }, []);
  const page = route === '/' ? <OverviewPage />
    : route === '/projects' ? <ProjectsPage />
      : route === '/networks' ? <NetworksPage />
        : route === '/routers' ? <RoutersPage />
          : route === '/ports' ? <PortsPage />
            : route === '/floating-ips' ? <FloatingIPsPage />
              : route === '/security-groups' ? <SecurityGroupsPage />
                : route === '/provider-networks' ? <ProviderNetworksPage />
                  : route === '/nodes' ? <NodesPage />
                    : route === '/operations' ? <OperationsPage />
                      : <OverviewPage />;
  return (
    <div className={`app-shell${embedded ? ' app-shell-embedded' : ''}`}>
      {!embedded && (
        <aside className={`sidebar${menuOpen ? ' sidebar-open' : ''}`}>
          <Brand />
          <nav aria-label="PVN sections">
            <span className="nav-caption">Cloud networking</span>
            {navigation.map((item) => (
              <a
                href={`#${item.to}`}
                key={item.to}
                onClick={() => setMenuOpen(false)}
                className={`nav-link${route === item.to ? ' active' : ''}`}
              >
                <span className="nav-glyph" aria-hidden="true">{item.glyph}</span>
                <span>{item.label}</span>
              </a>
            ))}
          </nav>
          <div className="sidebar-foot">
            <span className="connection-dot" aria-hidden="true" />
            <div><strong>Local manager</strong><span>Encrypted session</span></div>
          </div>
        </aside>
      )}
      {!embedded && menuOpen && <button className="sidebar-scrim" aria-label="Close navigation" onClick={() => setMenuOpen(false)} />}
      <div className="workspace">
        <header className="topbar">
          {!embedded && <button className="mobile-menu" aria-label="Open navigation" onClick={() => setMenuOpen(true)}>☰</button>}
          <div className="breadcrumb"><span>{session.cluster || 'PVE cluster'}</span><b>/</b><strong>PVN</strong></div>
          <div className="user-chip">
            <span className="user-avatar" aria-hidden="true">{session.user.slice(0, 1).toUpperCase()}</span>
            <div><strong>{session.user}</strong><span>PVE session</span></div>
          </div>
        </header>
        <main className="content">
          {page}
        </main>
      </div>
    </div>
  );
}

function Brand(_: PropsWithChildren) {
  return (
    <div className="brand">
      <span className="brand-mark" aria-hidden="true"><i /><i /><i /></span>
      <div><strong>PVN</strong><span>Proxmox Virtual Network</span></div>
    </div>
  );
}
