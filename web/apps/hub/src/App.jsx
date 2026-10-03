import React, { useState, useEffect } from 'react';
import { Card, PageHeader, Pill, Stat, Button, Empty, Row } from '@nexus/ui';
import { Server, RefreshCw, Radio, Cpu, Monitor } from 'lucide-react';

export default function App() {
  const [status, setStatus] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);

  const fetchStatus = async () => {
    try {
      const res = await fetch('/hub/status');
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
      const data = await res.json();
      setStatus(data);
      setError(null);
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchStatus();
    const interval = setInterval(fetchStatus, 3000);
    return () => clearInterval(interval);
  }, []);

  const edges = status?.edges || [];

  return (
    <div className="layout" style={{ maxWidth: 960, margin: '0 auto', padding: '24px 16px' }}>
      <PageHeader
        title="Nexus Hub"
        subtitle="Coordinated multi-device mesh status"
        status={
          status ? (
            <Pill tone="ok" pulse>
              online
            </Pill>
          ) : (
            <Pill tone="neutral">connecting...</Pill>
          )
        }
      >
        <Button
          variant="secondary"
          icon={RefreshCw}
          busy={loading}
          onClick={fetchStatus}
        >
          Refresh
        </Button>
      </PageHeader>

      {error && (
        <div style={{ marginBottom: 16 }}>
          <Pill tone="danger">Hub offline: {error}</Pill>
        </div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 16, marginBottom: 24 }}>
        <Card padded>
          <Stat label="Hub Version" value={status?.version || 'dev'} hint="Coordinator binary" />
        </Card>
        <Card padded>
          <Stat label="Active Edges" value={edges.length} hint="Connected satellites & boards" />
        </Card>
        <Card padded>
          <Stat label="Browsers" value={status?.browsers ?? 0} hint="Web clients attached" />
        </Card>
        <Card padded>
          <Stat
            label="Audio Features"
            value={
              [status?.voice && 'Voice', status?.music && 'Music'].filter(Boolean).join(' + ') || 'None'
            }
            hint="Hub audio pipelines"
          />
        </Card>
      </div>

      {status?.board && (
        <Card title="Primary Board" icon={Cpu} style={{ marginBottom: 24 }}>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: 12 }}>
            <Row label="ID">{status.board.id}</Row>
            <Row label="Name">{status.board.name || 'Unnamed'}</Row>
            <Row label="Firmware">{status.board.fw || 'unknown'}</Row>
            <Row label="API Base">{status.board.api || 'n/a'}</Row>
            <Row label="Uptime">{status.board.connected_s ? `${status.board.connected_s}s` : '0s'}</Row>
          </div>
        </Card>
      )}

      <Card title="Connected Edges" icon={Server}>
        {edges.length === 0 ? (
          <Empty icon={Radio} title="No edges connected">
            Edges (satellites and boards) connecting to /edge will appear here in real-time.
          </Empty>
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
            {edges.map((e) => (
              <div
                key={e.id}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'space-between',
                  padding: '12px 16px',
                  background: 'var(--surface-2)',
                  borderRadius: 'var(--radius-sm)',
                  border: '1px solid var(--border)',
                }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
                  <Radio size={18} style={{ color: 'var(--accent)' }} />
                  <div>
                    <div style={{ fontWeight: 600, display: 'flex', alignItems: 'center', gap: 8 }}>
                      <span>{e.name || e.id}</span>
                      <Pill tone="ok">{e.kind || 'edge'}</Pill>
                    </div>
                    <div style={{ fontSize: 12, color: 'var(--text-3)', fontFamily: 'var(--mono)' }}>
                      id: {e.id} {e.roles ? `· roles: ${e.roles}` : ''} {e.fw ? `· fw: ${e.fw}` : ''}
                    </div>
                  </div>
                </div>
                <div style={{ textAlign: 'right', fontSize: 13, color: 'var(--text-2)' }}>
                  Connected {e.connected_s || 0}s
                </div>
              </div>
            ))}
          </div>
        )}
      </Card>
    </div>
  );
}
