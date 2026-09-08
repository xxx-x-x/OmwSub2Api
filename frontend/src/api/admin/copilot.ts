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
  status: string
  access_token?: string
  error?: string
  error_description?: string
}

export async function startDeviceFlow(): Promise<CopilotDeviceCodeResponse> {
  const { data } = await apiClient.post<CopilotDeviceCodeResponse>('/admin/copilot/oauth/device-code')
  return data
}

export async function pollDeviceFlow(payload: CopilotPollRequest): Promise<CopilotPollResponse> {
  const { data } = await apiClient.post<CopilotPollResponse>('/admin/copilot/oauth/poll', payload)
  return data
}

export default { startDeviceFlow, pollDeviceFlow }
