import React, { useState } from 'react';
import { useAuth } from '../AuthContext';
import { Card, PageHeader, Field, Button, Banner } from '@nexus/ui';
import { LogIn, KeyRound } from 'lucide-react';

export function Login({ onNavigate }) {
  const { login, updatePassword, mustChange } = useAuth();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  // Forced password change state
  const [currentPass, setCurrentPass] = useState('');
  const [newPass, setNewPass] = useState('');
  const [confirmPass, setConfirmPass] = useState('');
  const [changeBusy, setChangeBusy] = useState(false);
  const [changeError, setChangeError] = useState(null);

  const handleLogin = async (e) => {
    e.preventDefault();
    setError(null);
    setBusy(true);

    try {
      const user = await login(username.trim(), password);
      if (!user.must_change) {
        onNavigate('/devices');
      } else {
        setCurrentPass(password); // Pre-fill current temporary password
      }
    } catch (err) {
      setError(err.message || 'Invalid username or password');
    } finally {
      setBusy(false);
    }
  };

  const handleChangePassword = async (e) => {
    e.preventDefault();
    setChangeError(null);

    if (newPass.length < 8) {
      setChangeError('New password must be at least 8 characters');
      return;
    }
    if (newPass !== confirmPass) {
      setChangeError('New passwords do not match');
      return;
    }

    setChangeBusy(true);
    try {
      await updatePassword(currentPass, newPass);
      onNavigate('/devices');
    } catch (err) {
      setChangeError(err.message || 'Failed to update password');
    } finally {
      setChangeBusy(false);
    }
  };

  if (mustChange) {
    return (
      <div style={{ maxWidth: 440, margin: '60px auto', padding: '0 16px' }}>
        <PageHeader
          title="Change Password"
          subtitle="A password update is required before accessing your account"
        />

        <Card padded>
          <form onSubmit={handleChangePassword} style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
            {changeError && <Banner tone="danger">{changeError}</Banner>}

            <Field label="Current / Temporary Password">
              <input
                type="password"
                required
                value={currentPass}
                onChange={(e) => setCurrentPass(e.target.value)}
                placeholder="••••••••"
                style={{ width: '100%' }}
              />
            </Field>

            <Field label="New Password" hint="At least 8 characters">
              <input
                type="password"
                required
                autoFocus
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

            <Button
              type="submit"
              variant="primary"
              busy={changeBusy}
              icon={KeyRound}
              style={{ marginTop: 8 }}
            >
              Update Password & Continue
            </Button>
          </form>
        </Card>
      </div>
    );
  }

  return (
    <div style={{ maxWidth: 420, margin: '60px auto', padding: '0 16px' }}>
      <PageHeader
        title="Sign In"
        subtitle="Access your Nexus Hub devices and dashboard"
      />

      <Card padded>
        <form onSubmit={handleLogin} style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          {error && <Banner tone="danger">{error}</Banner>}

          <Field label="Username">
            <input
              type="text"
              required
              autoFocus
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="Username"
              style={{ width: '100%' }}
            />
          </Field>

          <Field label="Password">
            <input
              type="password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="Password"
              style={{ width: '100%' }}
            />
          </Field>

          <Button
            type="submit"
            variant="primary"
            busy={busy}
            icon={LogIn}
            style={{ marginTop: 8 }}
          >
            Sign In
          </Button>
        </form>
      </Card>
    </div>
  );
}
