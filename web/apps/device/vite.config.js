import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// https://vitejs.dev/config/
export default defineConfig({
  base: './',
  plugins: [react()],
  define: {
    'import.meta.env.VITE_NEXUS_HOST': JSON.stringify(process.env.VITE_NEXUS_HOST || process.env.NEXUS_HOST || ''),
  },
  server: {
    port: 5173,
    host: true,
  },
});
