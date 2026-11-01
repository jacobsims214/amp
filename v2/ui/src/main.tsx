import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'
import { capturePairingFragment } from './remote/pairingLink'

// Before anything else reads the URL: take the remote-control pairing secret out of a
// /sessions/:id#p=…&h=… link and scrub it from the address bar and history (protocol 7.1).
capturePairingFragment()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
