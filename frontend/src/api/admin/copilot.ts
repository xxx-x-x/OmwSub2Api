/**
 * Admin Copilot API endpoints
 * Handles GitHub Copilot Device OAuth flows for administrators
 */

import { apiClient } from '../client'

export interface CopilotDeviceCodeResponse {
  session_id: string
  user_code: string
  verification_uri: string
  expires_in: number
  interval: number
}

export interface CopilotPollRequest {
  session_id: string
}

export interface CopilotPollResponse {
  status: 'pending' | 'slow_down' | 'complete' | string
  message?: string
  github_token?: string
  github_login?: string
  github_name?: string
  github_id?: number
  access_token?: string
  error?: string
  error_description?: string
}

export async function startDeviceFlow(): Promise<CopilotDeviceCodeResponse> {
  const { data } = await apiClient.post<CopilotDeviceCodeResponse>('/admin/copilot/oauth/device-code')
  return data
}

export async function pollDeviceFlow(sessionIdOrPayload: string | CopilotPollRequest): Promise<CopilotPollResponse> {
  const payload = typeof sessionIdOrPayload === 'string'
    ? { session_id: sessionIdOrPayload }
    : sessionIdOrPayload
  const { data } = await apiClient.post<CopilotPollResponse>('/admin/copilot/oauth/poll', payload)
  return data
}

export default { startDeviceFlow, pollDeviceFlow }
