import React, { useState, useEffect } from 'react';
import { useAuth } from '../AuthContext';
import { Card, PageHeader, Pill, Button, IconButton, Banner, Field } from '@nexus/ui';
import { Modal } from '../components/Modal';
import { getUsers, createUser, resetUserPassword, deleteUser } from '../api';
import { UserPlus, RefreshCw, KeyRound, Trash2, Copy, Check, ShieldAlert, Key } from 'lucide-react';

export function AdminUsers({ onNavigate }) {
  const { user: currentUser } = useAuth();
  const [users, setUsers] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);

  // Add User modal state
  const [isAddOpen, setIsAddOpen] = useState(false);
  const [newUsername, setNewUsername] = useState('');
  const [newDisplayName, setNewDisplayName] = useState('');
  const [newRole, setNewRole] = useState('member');
  const [addBusy, setAddBusy] = useState(false);
  const [addError, setAddError] = useState(null);

  // Temporary password modal state (shown once)
  const [tempPasswordModal, setTempPasswordModal] = useState(null); // { username, tempPassword }
  const [copied, setCopied] = useState(false);

  // Delete user confirmation state
  const [userToDelete, setUserToDelete] = useState(null);
  const [deleteBusy, setDeleteBusy] = useState(false);

  // Reset password confirmation state
  const [userToReset, setUserToReset] = useState(null);
  const [resetBusy, setResetBusy] = useState(false);

  const fetchUsers = async () => {
    setLoading(true);
    try {
      const data = await getUsers();
      setUsers(data || []);
      setError(null);
    } catch (err) {
      setError(err.message || 'Failed to load users');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (currentUser?.role === 'admin') {
      fetchUsers();
    }
  }, [currentUser]);

  if (currentUser?.role !== 'admin') {
    return (
      <div style={{ maxWidth: 600, margin: '60px auto', padding: '0 16px' }}>
        <Banner tone="danger">Administrator privileges required to access user management.</Banner>
        <div style={{ marginTop: 16 }}>
          <Button variant="secondary" onClick={() => onNavigate('/devices')}>
            Back to Devices
          </Button>
        </div>
      </div>
    );
  }

  const handleCreateUser = async (e) => {
    e.preventDefault();
    setAddError(null);
    if (!newUsername.trim()) {
      setAddError('Username is required');
      return;
    }

    setAddBusy(true);
    try {
      const res = await createUser({
        username: newUsername.trim(),
        display_name: newDisplayName.trim() || newUsername.trim(),
        role: newRole,
      });
      setIsAddOpen(false);
      setNewUsername('');
      setNewDisplayName('');
      setNewRole('member');

      // Show temporary password modal once
      setTempPasswordModal({
        username: res.user.username,
        tempPassword: res.temp_password,
      });

      await fetchUsers();
    } catch (err) {
      setAddError(err.message || 'Failed to create user');
    } finally {
      setAddBusy(false);
    }
  };

  const handleResetPassword = async () => {
    if (!userToReset) return;
    setResetBusy(true);
    try {
      const res = await resetUserPassword(userToReset.id);
      const targetUser = userToReset;
      setUserToReset(null);

      // Show temporary password modal once
      setTempPasswordModal({
        username: targetUser.username,
        tempPassword: res.temp_password,
      });

      await fetchUsers();
    } catch (err) {
      setError(err.message || 'Failed to reset password');
    } finally {
      setResetBusy(false);
    }
  };

  const handleDeleteUser = async () => {
    if (!userToDelete) return;
    setDeleteBusy(true);
    try {
      await deleteUser(userToDelete.id);
      setUserToDelete(null);
      await fetchUsers();
    } catch (err) {
      setError(err.message || 'Failed to delete user');
    } finally {
      setDeleteBusy(false);
    }
  };

  const handleCopyPassword = () => {
    if (tempPasswordModal?.tempPassword) {
      navigator.clipboard.writeText(tempPasswordModal.tempPassword);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  };

  const adminCount = users.filter((u) => u.role === 'admin').length;

  return (
    <div className="layout" style={{ maxWidth: 960, margin: '0 auto', padding: '0 16px 40px' }}>
      <PageHeader
        title="User Accounts"
        subtitle="Manage hub members, roles, and security access"
      >
        <div style={{ display: 'flex', gap: 10 }}>
          <Button
            variant="secondary"
            icon={RefreshCw}
            busy={loading}
            onClick={fetchUsers}
          >
            Refresh
          </Button>
          <Button
            variant="primary"
            icon={UserPlus}
            onClick={() => setIsAddOpen(true)}
          >
            Add User
          </Button>
        </div>
      </PageHeader>

      {error && <Banner tone="danger" style={{ marginBottom: 20 }}>{error}</Banner>}

      <Card padded>
        <div style={{ overflowX: 'auto' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse', textAlign: 'left', fontSize: 13 }}>
            <thead>
              <tr style={{ borderBottom: '1px solid var(--border)', color: 'var(--text-3)' }}>
                <th style={{ padding: '10px 14px', fontWeight: 600 }}>Username</th>
                <th style={{ padding: '10px 14px', fontWeight: 600 }}>Display Name</th>
                <th style={{ padding: '10px 14px', fontWeight: 600 }}>Role</th>
                <th style={{ padding: '10px 14px', fontWeight: 600 }}>Status</th>
                <th style={{ padding: '10px 14px', fontWeight: 600, textAlign: 'right' }}>Actions</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => {
                const isSelf = u.id === currentUser?.id;
                const isLastAdmin = u.role === 'admin' && adminCount <= 1;
                return (
                  <tr key={u.id} style={{ borderBottom: '1px solid var(--border)' }}>
                    <td style={{ padding: '12px 14px', fontWeight: 600, fontFamily: 'var(--mono)' }}>
                      {u.username}
                      {isSelf && <span style={{ marginLeft: 6, fontSize: 11, color: 'var(--accent)' }}>(you)</span>}
                    </td>
                    <td style={{ padding: '12px 14px' }}>{u.display_name || u.username}</td>
                    <td style={{ padding: '12px 14px' }}>
                      <Pill tone={u.role === 'admin' ? 'ok' : 'neutral'}>
                        {u.role}
                      </Pill>
                    </td>
                    <td style={{ padding: '12px 14px' }}>
                      {u.must_change ? (
                        <Pill tone="warn">must change password</Pill>
                      ) : (
                        <span style={{ color: 'var(--text-3)' }}>active</span>
                      )}
                    </td>
                    <td style={{ padding: '12px 14px', textAlign: 'right' }}>
                      <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 6 }}>
                        <Button
                          variant="secondary"
                          size="sm"
                          icon={KeyRound}
                          onClick={() => setUserToReset(u)}
                          title="Reset Password"
                        >
                          Reset
                        </Button>
                        <Button
                          variant="danger"
                          size="sm"
                          icon={Trash2}
                          onClick={() => setUserToDelete(u)}
                          disabled={isSelf || isLastAdmin}
                          title={isSelf ? 'Cannot delete self' : isLastAdmin ? 'Cannot delete last admin' : 'Delete user'}
                        >
                          Delete
                        </Button>
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </Card>

      {/* Add User Modal */}
      <Modal
        title="Add User"
        isOpen={isAddOpen}
        onClose={() => setIsAddOpen(false)}
        maxWidth={440}
      >
        <form onSubmit={handleCreateUser} style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          {addError && <Banner tone="danger">{addError}</Banner>}

          <Field label="Username">
            <input
              type="text"
              required
              autoFocus
              value={newUsername}
              onChange={(e) => setNewUsername(e.target.value)}
              placeholder="e.g. alex"
              style={{ width: '100%' }}
            />
          </Field>

          <Field label="Display Name">
            <input
              type="text"
              value={newDisplayName}
              onChange={(e) => setNewDisplayName(e.target.value)}
              placeholder="e.g. Alex Rivera"
              style={{ width: '100%' }}
            />
          </Field>

          <Field label="Role">
            <select
              value={newRole}
              onChange={(e) => setNewRole(e.target.value)}
              style={{ width: '100%', height: 38 }}
            >
              <option value="member">Member (Access own devices)</option>
              <option value="admin">Administrator (Full control)</option>
            </select>
          </Field>

          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 10, marginTop: 8 }}>
            <Button variant="secondary" onClick={() => setIsAddOpen(false)}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" busy={addBusy}>
              Create User
            </Button>
          </div>
        </form>
      </Modal>

      {/* Temporary Password Modal (Shown Once) */}
      <Modal
        title="Temporary Password Generated"
        isOpen={!!tempPasswordModal}
        onClose={() => setTempPasswordModal(null)}
        maxWidth={460}
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          <Banner tone="warn" icon={Key}>
            Provide this temporary password to <strong>{tempPasswordModal?.username}</strong>.
            They will be prompted to choose a new password upon signing in.
            <div style={{ marginTop: 6, fontWeight: 600 }}>This password will never be shown again.</div>
          </Banner>

          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'space-between',
              padding: '12px 16px',
              background: 'var(--surface-2)',
              borderRadius: 'var(--radius)',
              border: '1px solid var(--border)',
            }}
          >
            <span
              style={{
                fontFamily: 'var(--mono)',
                fontSize: 16,
                fontWeight: 700,
                color: 'var(--text)',
                letterSpacing: '0.04em',
              }}
            >
              {tempPasswordModal?.tempPassword}
            </span>
            <Button
              variant="secondary"
              size="sm"
              icon={copied ? Check : Copy}
              onClick={handleCopyPassword}
            >
              {copied ? 'Copied' : 'Copy'}
            </Button>
          </div>

          <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
            <Button variant="primary" onClick={() => setTempPasswordModal(null)}>
              Done
            </Button>
          </div>
        </div>
      </Modal>

      {/* Reset Password Confirmation Modal */}
      <Modal
        title="Reset User Password"
        isOpen={!!userToReset}
        onClose={() => setUserToReset(null)}
        maxWidth={440}
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          <p style={{ margin: 0, fontSize: 14 }}>
            Reset password for user <strong>{userToReset?.username}</strong>?
            All existing sessions for this user will be revoked immediately and a new temporary password will be created.
          </p>
          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 10 }}>
            <Button variant="secondary" onClick={() => setUserToReset(null)}>
              Cancel
            </Button>
            <Button variant="danger" busy={resetBusy} onClick={handleResetPassword}>
              Reset Password
            </Button>
          </div>
        </div>
      </Modal>

      {/* Delete User Confirmation Modal */}
      <Modal
        title="Delete User"
        isOpen={!!userToDelete}
        onClose={() => setUserToDelete(null)}
        maxWidth={440}
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          <Banner tone="danger" icon={ShieldAlert}>
            Are you sure you want to delete user <strong>{userToDelete?.username}</strong>?
            All sessions and pairing codes associated with this user will be deleted.
          </Banner>
          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 10 }}>
            <Button variant="secondary" onClick={() => setUserToDelete(null)}>
              Cancel
            </Button>
            <Button variant="danger" busy={deleteBusy} onClick={handleDeleteUser}>
              Delete User
            </Button>
          </div>
        </div>
      </Modal>
    </div>
  );
}
