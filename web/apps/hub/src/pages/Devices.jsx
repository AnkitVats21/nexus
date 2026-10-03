import React, { useState, useEffect, useRef } from 'react';
import { Card, PageHeader, Pill, Button, IconButton, Empty, Row, Banner, Field } from '@nexus/ui';
import { Modal } from '../components/Modal';
import { getDevices, patchDevice, deleteDevice, createPairingCode } from '../api';
import { Plus, RefreshCw, Cpu, ExternalLink, Edit2, Trash2, Check, X, ShieldAlert, Sparkles, Clock } from 'lucide-react';

export function Devices() {
  const [devices, setDevices] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);

  // Rename state
  const [renamingId, setRenamingId] = useState(null);
  const [renameVal, setRenameVal] = useState('');
  const [renameBusy, setRenameBusy] = useState(false);

  // Remove confirmation state
  const [deviceToRemove, setDeviceToRemove] = useState(null);
  const [removeBusy, setRemoveBusy] = useState(false);

  // Add Device modal state
  const [isPairingOpen, setIsPairingOpen] = useState(false);
  const [pairingCode, setPairingCode] = useState(null);
  const [expiresAt, setExpiresAt] = useState(null);
  const [pairingBusy, setPairingBusy] = useState(false);
  const [pairingError, setPairingError] = useState(null);
  const [pairedDeviceName, setPairedDeviceName] = useState(null);
  const [secondsRemaining, setSecondsRemaining] = useState(0);

  const initialDevicesRef = useRef(new Set());

  const fetchDevices = async (silently = false) => {
    if (!silently) setLoading(true);
    try {
      const data = await getDevices();
      setDevices(data || []);
      setError(null);

      // Check if a new device was paired during add-device modal
      if (isPairingOpen && initialDevicesRef.current.size > 0 && data) {
        for (const dev of data) {
          if (!initialDevicesRef.current.has(dev.id)) {
            setPairedDeviceName(dev.name || dev.id);
            break;
          }
        }
      }
    } catch (err) {
      if (!silently) setError(err.message || 'Failed to load devices');
    } finally {
      if (!silently) setLoading(false);
    }
  };

  useEffect(() => {
    fetchDevices();
    const interval = setInterval(() => fetchDevices(true), 3000);
    return () => clearInterval(interval);
  }, [isPairingOpen]);

  // Countdown timer for pairing code
  useEffect(() => {
    if (!expiresAt) return;
    const updateCountdown = () => {
      const now = Math.floor(Date.now() / 1000);
      const diff = Math.max(0, expiresAt - now);
      setSecondsRemaining(diff);
    };
    updateCountdown();
    const interval = setInterval(updateCountdown, 1000);
    return () => clearInterval(interval);
  }, [expiresAt]);

  const handleOpenPairing = async () => {
    setPairingBusy(true);
    setPairingError(null);
    setPairedDeviceName(null);
    setIsPairingOpen(true);

    // Snapshot existing device IDs
    initialDevicesRef.current = new Set(devices.map((d) => d.id));

    try {
      const res = await createPairingCode();
      setPairingCode(res.code);
      setExpiresAt(res.expires_at);
    } catch (err) {
      setPairingError(err.message || 'Failed to generate pairing code');
    } finally {
      setPairingBusy(false);
    }
  };

  const handleStartRename = (dev) => {
    setRenamingId(dev.id);
    setRenameVal(dev.name || dev.id);
  };

  const handleSaveRename = async (id) => {
    if (!renameVal.trim()) return;
    setRenameBusy(true);
    try {
      await patchDevice(id, renameVal.trim());
      setDevices((prev) =>
        prev.map((d) => (d.id === id ? { ...d, name: renameVal.trim() } : d))
      );
      setRenamingId(null);
    } catch (err) {
      setError(err.message || 'Failed to rename device');
    } finally {
      setRenameBusy(false);
    }
  };

  const handleConfirmDelete = async () => {
    if (!deviceToRemove) return;
    setRemoveBusy(true);
    try {
      await deleteDevice(deviceToRemove.id);
      setDevices((prev) => prev.filter((d) => d.id !== deviceToRemove.id));
      setDeviceToRemove(null);
    } catch (err) {
      setError(err.message || 'Failed to remove device');
    } finally {
      setRemoveBusy(false);
    }
  };

  const formatCountdown = (secs) => {
    const mins = Math.floor(secs / 60);
    const s = secs % 60;
    return `${mins}:${s < 10 ? '0' : ''}${s}`;
  };

  const formatTime = (ts) => {
    if (!ts) return 'Never';
    const now = Math.floor(Date.now() / 1000);
    const diff = Math.max(0, now - ts);
    if (diff < 60) return `${diff}s ago`;
    if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
    if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
    return new Date(ts * 1000).toLocaleDateString();
  };

  const formatUptime = (secs) => {
    if (!secs) return '0s';
    if (secs < 60) return `${secs}s`;
    if (secs < 3600) return `${Math.floor(secs / 60)}m ${secs % 60}s`;
    return `${Math.floor(secs / 3600)}h ${Math.floor((secs % 3600) / 60)}m`;
  };

  return (
    <div className="layout" style={{ maxWidth: 1040, margin: '0 auto', padding: '0 16px 40px' }}>
      <PageHeader
        title="Devices"
        subtitle="Manage paired smart speakers, satellites, and edge nodes"
      >
        <div style={{ display: 'flex', gap: 10 }}>
          <Button
            variant="secondary"
            icon={RefreshCw}
            busy={loading}
            onClick={() => fetchDevices()}
          >
            Refresh
          </Button>
          <Button
            variant="primary"
            icon={Plus}
            onClick={handleOpenPairing}
          >
            Add Device
          </Button>
        </div>
      </PageHeader>

      {error && <Banner tone="danger" style={{ marginBottom: 20 }}>{error}</Banner>}

      {devices.length === 0 && !loading ? (
        <Empty icon={Cpu} title="No devices paired yet">
          Click <strong>Add Device</strong> above to generate a 6-digit pairing code for your board or Orbit satellite.
        </Empty>
      ) : (
        <div
          style={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fill, minmax(320px, 1fr))',
            gap: 18,
          }}
        >
          {devices.map((dev) => (
            <Card key={dev.id} padded>
              <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 14 }}>
                <div style={{ flex: 1, minWidth: 0, marginRight: 10 }}>
                  {renamingId === dev.id ? (
                    <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                      <input
                        type="text"
                        autoFocus
                        value={renameVal}
                        onChange={(e) => setRenameVal(e.target.value)}
                        style={{ height: 32, fontSize: 13, flex: 1 }}
                      />
                      <IconButton
                        icon={Check}
                        label="Save"
                        onClick={() => handleSaveRename(dev.id)}
                        disabled={renameBusy}
                        size={16}
                      />
                      <IconButton
                        icon={X}
                        label="Cancel"
                        onClick={() => setRenamingId(null)}
                        size={16}
                      />
                    </div>
                  ) : (
                    <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                      <h3
                        style={{
                          margin: 0,
                          fontSize: 16,
                          fontWeight: 600,
                          overflow: 'hidden',
                          textOverflow: 'ellipsis',
                          whiteSpace: 'nowrap',
                        }}
                      >
                        {dev.name || dev.id}
                      </h3>
                      <IconButton
                        icon={Edit2}
                        label="Rename"
                        onClick={() => handleStartRename(dev)}
                        size={14}
                      />
                    </div>
                  )}
                  <span style={{ fontSize: 12, color: 'var(--text-3)', fontFamily: 'var(--mono)' }}>
                    {dev.id}
                  </span>
                </div>

                <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-end', gap: 4 }}>
                  {dev.online ? (
                    <Pill tone="ok" pulse>online</Pill>
                  ) : (
                    <Pill tone="neutral">offline</Pill>
                  )}
                  {dev.legacy && (
                    <Pill tone="warn">Legacy: pair again</Pill>
                  )}
                </div>
              </div>

              <div style={{ display: 'flex', flexDirection: 'column', gap: 6, marginBottom: 16, fontSize: 13 }}>
                <Row label="Kind">{dev.kind || 'unknown'}</Row>
                {dev.fw && <Row label="Firmware">{dev.fw}</Row>}
                {dev.uplink && <Row label="Uplink">{dev.uplink}</Row>}
                {dev.owner && <Row label="Owner">{dev.owner}</Row>}
                <Row label="Last Seen">{formatTime(dev.last_seen)}</Row>
                {dev.online && dev.connected_s > 0 && (
                  <Row label="Connected">{formatUptime(dev.connected_s)}</Row>
                )}
              </div>

              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', paddingTop: 12, borderTop: '1px solid var(--border)' }}>
                <a
                  href={`/hub/d/${encodeURIComponent(dev.id)}/`}
                  target="_blank"
                  rel="noreferrer"
                  style={{ textDecoration: 'none' }}
                >
                  <Button variant="secondary" size="sm" icon={ExternalLink}>
                    Open App
                  </Button>
                </a>

                <Button
                  variant="danger"
                  size="sm"
                  icon={Trash2}
                  onClick={() => setDeviceToRemove(dev)}
                >
                  Remove
                </Button>
              </div>
            </Card>
          ))}
        </div>
      )}

      {/* Remove Confirmation Dialog */}
      <Modal
        title="Remove Device"
        isOpen={!!deviceToRemove}
        onClose={() => setDeviceToRemove(null)}
        maxWidth={440}
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          <Banner tone="warn" icon={ShieldAlert}>
            Are you sure you want to remove <strong>{deviceToRemove?.name || deviceToRemove?.id}</strong>?
            Its device token will be revoked and any active connection will be closed.
          </Banner>
          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 10 }}>
            <Button variant="secondary" onClick={() => setDeviceToRemove(null)}>
              Cancel
            </Button>
            <Button variant="danger" busy={removeBusy} onClick={handleConfirmDelete}>
              Confirm Remove
            </Button>
          </div>
        </div>
      </Modal>

      {/* Add Device Pairing Modal */}
      <Modal
        title="Add Device"
        isOpen={isPairingOpen}
        onClose={() => setIsPairingOpen(false)}
        maxWidth={500}
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 20 }}>
          {pairedDeviceName ? (
            <div style={{ textAlign: 'center', padding: '16px 0' }}>
              <div
                style={{
                  width: 54,
                  height: 54,
                  borderRadius: '50%',
                  background: 'var(--ok-soft)',
                  color: 'var(--ok)',
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'center',
                  margin: '0 auto 16px',
                }}
              >
                <Sparkles size={28} />
              </div>
              <h3 style={{ margin: '0 0 6px', fontSize: 20, fontWeight: 700 }}>
                Paired: {pairedDeviceName}
              </h3>
              <p style={{ margin: 0, color: 'var(--text-2)', fontSize: 14 }}>
                Device registered and connected to your Nexus Hub.
              </p>
              <Button
                variant="primary"
                onClick={() => setIsPairingOpen(false)}
                style={{ marginTop: 24, minWidth: 120 }}
              >
                Done
              </Button>
            </div>
          ) : (
            <>
              {pairingError && <Banner tone="danger">{pairingError}</Banner>}

              <div
                style={{
                  textAlign: 'center',
                  padding: '20px 16px',
                  background: 'var(--surface-2)',
                  borderRadius: 'var(--radius)',
                  border: '1px solid var(--border)',
                }}
              >
                <span style={{ fontSize: 13, color: 'var(--text-3)', fontWeight: 500 }}>
                  PAIRING CODE
                </span>
                <div
                  style={{
                    fontFamily: 'var(--mono)',
                    fontSize: 42,
                    fontWeight: 800,
                    letterSpacing: '0.18em',
                    color: 'var(--accent)',
                    margin: '10px 0',
                  }}
                >
                  {pairingCode || '------'}
                </div>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 6, fontSize: 13, color: 'var(--text-2)' }}>
                  <Clock size={14} />
                  <span>Expires in {formatCountdown(secondsRemaining)}</span>
                </div>
              </div>

              <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
                <h4 style={{ margin: 0, fontSize: 14, fontWeight: 600 }}>Pairing Steps</h4>

                <div style={{ background: 'var(--surface-3)', padding: 12, borderRadius: 'var(--radius-sm)', fontSize: 13 }}>
                  <strong style={{ display: 'block', marginBottom: 4 }}>ESP32 Waveshare Board:</strong>
                  <ol style={{ margin: 0, paddingLeft: 18, color: 'var(--text-2)', lineHeight: 1.6 }}>
                    <li>Open your board's dashboard (via IP or <code>nexus.local</code>).</li>
                    <li>Navigate to <strong>Settings</strong> → <strong>Hub</strong>.</li>
                    <li>Enter the Hub Address and the 6-digit code above.</li>
                  </ol>
                </div>

                <div style={{ background: 'var(--surface-3)', padding: 12, borderRadius: 'var(--radius-sm)', fontSize: 13 }}>
                  <strong style={{ display: 'block', marginBottom: 4 }}>Orbit Satellite (Linux/Pi/PC):</strong>
                  <div style={{ fontFamily: 'var(--mono)', fontSize: 12, color: 'var(--text)', background: 'var(--surface)', padding: '6px 10px', borderRadius: 6, marginTop: 4 }}>
                    nexus-orbit --hub {window.location.host} --pair-code {pairingCode || '<code>'}
                  </div>
                </div>
              </div>

              <div style={{ display: 'flex', justifyContent: 'flex-end', paddingTop: 8 }}>
                <Button variant="secondary" onClick={() => setIsPairingOpen(false)}>
                  Close
                </Button>
              </div>
            </>
          )}
        </div>
      </Modal>
    </div>
  );
}
