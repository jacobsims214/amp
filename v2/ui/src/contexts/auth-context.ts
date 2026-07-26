import { createContext } from 'react'
import type { Me } from '../types'

export interface AuthState {
  me: Me | null
  loading: boolean
  error: string | null
  isAdmin: boolean
}

export const AuthContext = createContext<AuthState>({ me: null, loading: true, error: null, isAdmin: false })
