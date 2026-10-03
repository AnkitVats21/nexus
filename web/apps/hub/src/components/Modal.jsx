import React, { useEffect } from 'react';
import { X } from 'lucide-react';
import { IconButton } from '@nexus/ui';

export function Modal({ title, isOpen, onClose, children, maxWidth = 500 }) {
  useEffect(() => {
    if (!isOpen) return;
    const handleKeyDown = (e) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', handleKeyDown);
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, [isOpen, onClose]);

  if (!isOpen) return null;

  return (
    <div
      style={{
        position: 'fixed',
        inset: 0,
        backgroundColor: 'rgba(0, 0, 0, 0.65)',
        backdropFilter: 'blur(4px)',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        zIndex: 999,
        padding: 16,
      }}
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        className="card card-padded"
        style={{
          width: '100%',
          maxWidth,
          boxShadow: 'var(--shadow-lg)',
          animation: 'fadeIn 0.15s ease-out',
        }}
      >
        <header
          className="card-header"
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            marginBottom: 16,
          }}
        >
          {title && <h2 className="card-title" style={{ margin: 0 }}>{title}</h2>}
          <IconButton icon={X} label="Close" onClick={onClose} size={18} />
        </header>
        <div>{children}</div>
      </div>
    </div>
  );
}
