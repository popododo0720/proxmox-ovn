import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import { ApiProvider } from './api/context';
import './styles.css';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ApiProvider>
      <App />
    </ApiProvider>
  </StrictMode>,
);
