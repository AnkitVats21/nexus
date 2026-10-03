import React, { useState, useEffect } from 'react';
import { AuthProvider, useAuth } from './AuthContext';
import { Nav } from './components/Nav';
import { Setup } from './pages/Setup';
import { Login } from './pages/Login';
import { Devices } from './pages/Devices';
import { Account } from './pages/Account';
import { AdminUsers } from './pages/AdminUsers';
import { Loader2 } from 'lucide-react';

function AppContent() {
  const { user, loading, setupNeeded, mustChange } = useAuth();
  const [path, setPath] = useState(window.location.pathname || '/');

  useEffect(() => {
    const onPopState = () => {
      setPath(window.location.pathname || '/');
    };
    window.addEventListener('popstate', onPopState);
    return () => window.removeEventListener('popstate', onPopState);
  }, []);

  const navigate = (to) => {
    if (to !== window.location.pathname) {
      window.history.pushState({}, '', to);
      setPath(to);
    }
  };

  if (loading) {
    return (
      <div style={{ display: 'flex', height: '100vh', alignItems: 'center', justifyContent: 'center' }}>
        <Loader2 className="spin" size={32} style={{ color: 'var(--accent)' }} />
      </div>
    );
  }

  if (setupNeeded || path === '/setup') {
    return <Setup onNavigate={navigate} />;
  }

  if (!user || mustChange || path === '/login') {
    return <Login onNavigate={navigate} />;
  }

  let page = <Devices onNavigate={navigate} />;
  if (path === '/account') {
    page = <Account onNavigate={navigate} />;
  } else if (path === '/admin/users') {
    page = <AdminUsers onNavigate={navigate} />;
  } else if (path === '/devices' || path === '/') {
    page = <Devices onNavigate={navigate} />;
  }

  return (
    <div className="hub-app" style={{ minHeight: '100vh', display: 'flex', flexDirection: 'column' }}>
      <Nav currentPath={path} onNavigate={navigate} />
      <main style={{ flex: 1 }}>{page}</main>
    </div>
  );
}

export default function App() {
  return (
    <AuthProvider>
      <AppContent />
    </AuthProvider>
  );
}
