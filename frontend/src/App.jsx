import React from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { getToken } from './api'
import AppLayout from './layout/AppLayout'
import Login from './pages/Login'
import Vendors from './pages/Vendors'
import Assets from './pages/Assets'
import Events from './pages/Events'
import Scans from './pages/Scans'
import Tokens from './pages/Tokens'
import Docs from './pages/Docs'
import HostedRules from './pages/HostedRules'
import ProbeKeys from './pages/ProbeKeys'

function RequireAuth({ children }) {
  if (!getToken()) return <Navigate to="/login" replace />
  return children
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route
        path="/"
        element={
          <RequireAuth>
            <AppLayout />
          </RequireAuth>
        }
      >
        <Route index element={<Navigate to="/events" replace />} />
        <Route path="vendors" element={<Vendors />} />
        <Route path="assets" element={<Assets />} />
        <Route path="events" element={<Events />} />
        <Route path="scans" element={<Scans />} />
        <Route path="tokens" element={<Tokens />} />
        <Route path="docs" element={<Docs />} />
        <Route path="hosted-rules" element={<HostedRules />} />
        <Route path="probe-keys" element={<ProbeKeys />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
