import React, { useState } from 'react';
import { useAuth } from '../AuthContext';
import { Card, PageHeader, Field, Button, Banner } from '@nexus/ui';
import { ShieldCheck, ArrowRight } from 'lucide-react';

export function Setup({ onNavigate }) {
  const { setupAdmin } = useAuth();
  const [setupCode, setSetupCode] = useState('');
  const [username, setUsername] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [password, setPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  const handleSubmit = async (e) => {
    e.preventDefault();
    setError(null);

    if (!setupCode.trim()) {
      setError('Setup code is required');
      return;
    }
    if (!username.trim()) {
      setError('Username is required');
      return;
    }
    if (password.length < 8) {
      setError('Password must be at least 8 characters');
      return;
    }
    if (password !== confirmPassword) {
      setError('Passwords do not match');
      return;
    }

    setBusy(true);
    try {
      await setupAdmin({
        setup_code: setupCode.trim(),
        username: username.trim(),
        display_name: displayName.trim() || username.trim(),
        password,
      });
      onNavigate('/devices');
    } catch (err) {
      setError(err.message || 'Setup failed');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div style={{ maxWidth: 460, margin: '60px auto', padding: '0 16px' }}>
      <PageHeader
        title="Hub Setup"
        subtitle="Initialize administrator account for this Nexus Hub"
      />

      <Card padded>
        <form onSubmit={handleSubmit} style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          {error && <Banner tone="danger">{error}</Banner>}

          <Field
            label="Setup Code"
            hint="Printed in the hub console log or setup_code file"
          >
            <input
              type="text"
              required
              autoFocus
              value={setupCode}
              onChange={(e) => setSetupCode(e.target.value)}
              placeholder="e.g. 849201"
              style={{ width: '100%' }}
            />
          </Field>

          <Field label="Admin Username">
            <input
              type="text"
              required
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="admin"
              style={{ width: '100%' }}
            />
          </Field>

          <Field label="Display Name (optional)">
            <input
              type="text"
              value={displayName}
              onChange={(e) => setDisplayName(e.target.value)}
              placeholder="Admin"
              style={{ width: '100%' }}
            />
          </Field>

          <Field label="Admin Password" hint="At least 8 characters">
            <input
              type="password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="••••••••"
              style={{ width: '100%' }}
            />
          </Field>

          <Field label="Confirm Password">
            <input
              type="password"
              required
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
              placeholder="••••••••"
              style={{ width: '100%' }}
            />
          </Field>

          <Button
            type="submit"
            variant="primary"
            busy={busy}
            icon={ArrowRight}
            style={{ marginTop: 8 }}
          >
            Complete Setup
          </Button>
        </form>
      </Card>
    </div>
  );
}
