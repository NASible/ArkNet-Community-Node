import { useState } from 'react';
import Dashboard from './pages/Dashboard';
import Diagnostics from './pages/Diagnostics';
import Media from './pages/Media';
import Settings from './pages/Settings';
import Users from './pages/Users';

export default function App() {
  const [page, setPage] = useState<'Dashboard' | 'Media' | 'Users' | 'Diagnostics' | 'Settings'>('Dashboard');
  return (
    <>
      <header className="app-header"><div className="container"><strong>ArkNet</strong><span>Community Node</span></div></header>
      <main className="container">
        <nav className="page-nav" aria-label="Local management">
          {(['Dashboard', 'Media', 'Users', 'Diagnostics', 'Settings'] as const).map(name => <button type="button" key={name}
            aria-current={page === name ? 'page' : undefined} onClick={() => setPage(name)}>{name}</button>)}
        </nav>
        {page === 'Dashboard' ? <Dashboard /> : page === 'Media' ? <Media /> : page === 'Users' ? <Users /> : page === 'Diagnostics' ? <Diagnostics /> : <Settings />}
      </main>
    </>
  );
}
