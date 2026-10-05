/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import i18next from 'i18next'
import { beforeAll, describe, expect, test } from 'vitest'

import type { UsageLog } from '../../data/schema'
import type { LogOtherData } from '../../types'
import { DetailsDialog } from '../dialogs/details-dialog'

const i18nKeys = {
  'Log Details': 'Log Details',
  Error: 'Error',
  'Raw Request': 'Raw Request',
  Method: 'Method',
  'Request Headers': 'Request Headers',
  'Request Body': 'Request Body',
  'Body truncated, original size: {{bytes}} bytes':
    'Body truncated, original size: {{bytes}} bytes',
}

beforeAll(() => {
  i18next.addResourceBundle('en', 'translation', i18nKeys)
})

function makeLog(other: LogOtherData): UsageLog {
  return {
    id: 1,
    user_id: 1,
    created_at: 1,
    type: 5,
    content: 'status_code=400, bad request',
    username: 'user',
    token_name: 'token',
    model_name: 'glm-5.3',
    quota: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    use_time: 0,
    is_stream: false,
    channel: 150,
    channel_name: '',
    token_id: 1,
    group: 'default',
    ip: '',
    other: JSON.stringify(other),
    request_id: 'req-1',
    upstream_request_id: '',
  }
}

function renderDetails(other: LogOtherData, isAdmin = true) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const freshAt = Date.now() + 60_000
  queryClient.setQueryData(['status'], {}, { updatedAt: freshAt })
  queryClient.setQueryData(
    ['pricing'],
    { data: [], vendors: [] },
    { updatedAt: freshAt }
  )
  render(
    <QueryClientProvider client={queryClient}>
      <DetailsDialog
        log={makeLog(other)}
        isAdmin={isAdmin}
        isRoot={false}
        open
        onOpenChange={() => undefined}
      />
    </QueryClientProvider>
  )
  return queryClient
}

const rawRequest = {
  method: 'POST',
  url: '/v1/chat/completions',
  body: '{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}',
  body_bytes: 51,
  body_content_type: 'application/json',
  headers: {
    'User-Agent': 'OpenAI/Python 1.52.0',
    Authorization: '***',
  },
  attempts: 2,
}

describe('raw request section', () => {
  test('renders the captured request for administrators', () => {
    const queryClient = renderDetails({
      admin_info: { raw_request: rawRequest },
    })
    expect(screen.getByText('Raw Request')).toBeInTheDocument()
    // method and url render together on one row
    expect(screen.getByText('POST /v1/chat/completions')).toBeInTheDocument()
    // headers render one line each as "Name: value"
    expect(screen.getByText(/OpenAI\/Python 1\.52\.0/)).toBeInTheDocument()
    expect(screen.getByText(/^Authorization: \*\*\*$/)).toBeInTheDocument()
    // body renders inside <pre>
    expect(screen.getByText(rawRequest.body)).toBeInTheDocument()
    queryClient.clear()
  })

  test('flags a truncated body with its original size', () => {
    const queryClient = renderDetails({
      admin_info: {
        raw_request: { ...rawRequest, body_truncated: true, body_bytes: 4213 },
      },
    })
    expect(
      screen.getByText('Body truncated, original size: 4213 bytes')
    ).toBeInTheDocument()
    queryClient.clear()
  })

  test('hides the section from non-administrators', () => {
    const queryClient = renderDetails(
      { admin_info: { raw_request: rawRequest } },
      false
    )
    expect(screen.queryByText('Raw Request')).toBeNull()
    queryClient.clear()
  })

  test('renders nothing when the log has no captured request', () => {
    const queryClient = renderDetails({ admin_info: { use_channel: [150] } })
    expect(screen.queryByText('Raw Request')).toBeNull()
    queryClient.clear()
  })

  test('renders nothing when the log has no admin info at all', () => {
    const queryClient = renderDetails({})
    expect(screen.queryByText('Raw Request')).toBeNull()
    queryClient.clear()
  })
})
