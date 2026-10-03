import React, { createContext, useContext, useState, useEffect, useCallback } from 'react';
import { getMe, postLogin, postLogout, postSetup, changePassword as apiChangePassword, getSetupStatus } from './api';

const AuthContext = createContext(null);

export function AuthProvider({ children }) {
  const [user, setUser] = useState(null);
  const [loading, setLoading] = useState(true);
  const [setupNeeded, setSetupNeeded] = useState(false);
  const [mustChange, setMustChange] = useState(false);

  const checkAuth = useCallback(async () => {
    try {
      // Check setup status first if unauthenticated
      const setup = await getSetupStatus().catch(() => null);
      if (setup && setup.needed) {
        setSetupNeeded(true);
        setUser(null);
        setLoading(false);
        return;
      }
      setSetupNeeded(false);

      const me = await getMe();
      setUser(me);
      setMustChange(!!me.must_change);
    } catch (err) {
      setUser(null);
      if (err.status === 403 && err.mustChange) {
        setMustChange(true);
      }
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    checkAuth();

    const onUnauthorized = () => {
      setUser(null);
    };

    const onMustChange = () => {
      setMustChange(true);
    };

    window.addEventListener('hub:unauthorized', onUnauthorized);
    window.addEventListener('hub:must_change', onMustChange);
    return () => {
      window.removeEventListener('hub:unauthorized', onUnauthorized);
      window.removeEventListener('hub:must_change', onMustChange);
    };
  }, [checkAuth]);

  const login = async (username, password) => {
    const res = await postLogin({ username, password });
    setUser(res.user);
    setMustChange(!!res.user.must_change);
    return res.user;
  };

  const logout = async () => {
    try {
      await postLogout();
    } catch {
      // Ignore logout failure
    }
    setUser(null);
    setMustChange(false);
  };

  const setupAdmin = async (form) => {
    const res = await postSetup(form);
    setSetupNeeded(false);
    setUser(res.user);
    setMustChange(!!res.user.must_change);
    return res.user;
  };

  const updatePassword = async (current, next) => {
    await apiChangePassword(current, next);
    setMustChange(false);
    if (user) {
      setUser({ ...user, must_change: false });
    }
  };

  return (
    <AuthContext.Provider
      value={{
        user,
        loading,
        setupNeeded,
        mustChange,
        login,
        logout,
        setupAdmin,
        updatePassword,
        checkAuth,
      }}
    >
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error('useAuth must be used within an AuthProvider');
  }
  return ctx;
}
