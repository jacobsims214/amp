import { useContext } from 'react'
import { AuthContext } from '../contexts/auth-context'

export function useAuth() {
  return useContext(AuthContext)
}
