import React from 'react';
import { useAuth } from '../AuthContext';
import { Pill, Button } from '@nexus/ui';
import { Cpu, User, Users, LogOut } from 'lucide-react';

export function Nav({ currentPath, onNavigate }) {
  const { user, logout } = useAuth();

  if (!user) return null;

  const links = [
    { path: '/devices', label: 'Devices', icon: Cpu },
    { path: '/account', label: 'Account', icon: User },
  ];

  if (user.role === 'admin') {
    links.push({ path: '/admin/users', label: 'Users', icon: Users });
  }

  const handleSignOut = async () => {
    await logout();
    onNavigate('/login');
  };

  return (
    <nav
      style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        padding: '12px 24px',
        background: 'var(--surface)',
        borderBottom: '1px solid var(--border)',
        marginBottom: 24,
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 24 }}>
        <div
          onClick={() => onNavigate('/devices')}
          style={{
            cursor: 'pointer',
            display: 'flex',
            alignItems: 'center',
            gap: 8,
            fontWeight: 700,
            fontSize: 16,
            letterSpacing: '-0.02em',
          }}
        >
          <div
            style={{
              width: 24,
              height: 24,
              borderRadius: 6,
              background: 'linear-gradient(135deg, #6f7cff, #b07bff)',
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              color: '#fff',
              fontSize: 12,
              fontWeight: 800,
            }}
          >
            N
          </div>
          Nexus Hub
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          {links.map((link) => {
            const Icon = link.icon;
            const isActive = currentPath === link.path || (link.path === '/devices' && currentPath === '/');
            return (
              <button
                key={link.path}
                onClick={() => onNavigate(link.path)}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 6,
                  padding: '6px 12px',
                  borderRadius: 'var(--radius-sm)',
                  border: 'none',
                  background: isActive ? 'var(--surface-3)' : 'transparent',
                  color: isActive ? 'var(--text)' : 'var(--text-2)',
                  fontWeight: isActive ? 600 : 500,
                  fontSize: 13,
                  cursor: 'pointer',
                  transition: 'background 0.15s, color 0.15s',
                }}
              >
                <Icon size={15} />
                {link.label}
              </button>
            );
          })}
        </div>
      </div>

      <div style={{ display: 'flex', alignItems: 'center', gap: 14 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span style={{ fontSize: 13, fontWeight: 500, color: 'var(--text-2)' }}>
            {user.display_name || user.username}
          </span>
          <Pill tone={user.role === 'admin' ? 'ok' : 'neutral'}>
            {user.role}
          </Pill>
        </div>

        <Button
          variant="secondary"
          size="sm"
          icon={LogOut}
          onClick={handleSignOut}
          title="Sign Out"
        >
          Sign Out
        </Button>
      </div>
    </nav>
  );
}
