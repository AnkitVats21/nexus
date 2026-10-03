import React, { useState } from 'react';
import { useAuth } from '../AuthContext';
import { Card, PageHeader, Field, Button, Banner, Pill } from '@nexus/ui';
import { KeyRound, LogOut, CheckCircle2 } from 'lucide-react';
import { MIN_PASSWORD_LEN } from '../constants';

export function Account({ onNavigate }) {
  const { user, updatePassword, logout } = useAuth();
  const [currentPass, setCurrentPass] = useState('');
  const [newPass, setNewPass] = useState('');
  const [confirmPass, setConfirmPass] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const [success, setSuccess] = useState(false);

  const handleChangePassword = async (e) => {
    e.preventDefault();
    setError(null);
    setSuccess(false);

    if (newPass.length < MIN_PASSWORD_LEN) {
      setError(`New password must be at least ${MIN_PASSWORD_LEN} characters`);
      return;
    }
    if (newPass !== confirmPass) {
      setError('New passwords do not match');
      return;
    }

    setBusy(true);
    try {
      await updatePassword(currentPass, newPass);
      setSuccess(true);
      setCurrentPass('');
      setNewPass('');
      setConfirmPass('');
    } catch (err) {
      setError(err.message || 'Failed to update password');
    } finally {
      setBusy(false);
    }
  };

  const handleSignOut = async () => {
    await logout();
    onNavigate('/login');
  };

  return (
    <div className="layout" style={{ maxWidth: 600, margin: '0 auto', padding: '0 16px 40px' }}>
      <PageHeader
        title="Account"
        subtitle="Manage your profile and security credentials"
      />

      <div style={{ display: 'flex', flexDirection: 'column', gap: 20 }}>
        {/* User Info Card */}
        <Card title="Profile Details" padded>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span style={{ color: 'var(--text-2)', fontSize: 14 }}>Username</span>
              <span style={{ fontWeight: 600, fontFamily: 'var(--mono)' }}>{user?.username}</span>
            </div>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span style={{ color: 'var(--text-2)', fontSize: 14 }}>Display Name</span>
              <span style={{ fontWeight: 500 }}>{user?.display_name || user?.username}</span>
            </div>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span style={{ color: 'var(--text-2)', fontSize: 14 }}>Role</span>
              <Pill tone={user?.role === 'admin' ? 'ok' : 'neutral'}>
                {user?.role}
              </Pill>
            </div>
          </div>
        </Card>

        {/* Change Password Card */}
        <Card title="Change Password" padded>
          <form onSubmit={handleChangePassword} style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
            {error && <Banner tone="danger">{error}</Banner>}
            {success && (
              <Banner tone="ok" icon={CheckCircle2}>
                Password updated successfully. Other active sessions have been revoked.
              </Banner>
            )}

            <Field label="Current Password">
              <input
                type="password"
                required
                value={currentPass}
                onChange={(e) => setCurrentPass(e.target.value)}
                placeholder="••••••••"
                style={{ width: '100%' }}
              />
            </Field>

            <Field label="New Password" hint={`At least ${MIN_PASSWORD_LEN} characters`}>
              <input
                type="password"
                required
                value={newPass}
                onChange={(e) => setNewPass(e.target.value)}
                placeholder="••••••••"
                style={{ width: '100%' }}
              />
            </Field>

            <Field label="Confirm New Password">
              <input
                type="password"
                required
                value={confirmPass}
                onChange={(e) => setConfirmPass(e.target.value)}
                placeholder="••••••••"
                style={{ width: '100%' }}
              />
            </Field>

            <div style={{ display: 'flex', justifyContent: 'flex-start', marginTop: 4 }}>
              <Button
                type="submit"
                variant="primary"
                busy={busy}
                icon={KeyRound}
              >
                Update Password
              </Button>
            </div>
          </form>
        </Card>

        {/* Sign Out Card */}
        <Card title="Session" padded>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
            <div>
              <div style={{ fontWeight: 600, marginBottom: 2 }}>Sign out of this hub</div>
              <div style={{ fontSize: 13, color: 'var(--text-2)' }}>Clears the session cookie on this browser.</div>
            </div>
            <Button
              variant="danger"
              icon={LogOut}
              onClick={handleSignOut}
            >
              Sign Out
            </Button>
          </div>
        </Card>
      </div>
    </div>
  );
}
