import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import MemberLogin from './MemberLogin';
import './styles.css';

// Global members sign in at /member; ArkNet Core returns them to /login/callback.
const member = window.location.pathname === '/member' || window.location.pathname === '/login/callback';
createRoot(document.getElementById('root')!).render(<StrictMode>{member ? <MemberLogin /> : <App />}</StrictMode>);
