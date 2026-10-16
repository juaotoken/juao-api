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
import { render, screen, within } from '@testing-library/react'
import i18next from 'i18next'
import { afterEach, beforeAll, describe, expect, test } from 'vitest'

import type { UsageLog } from '../../data/schema'
import type { LogOtherData } from '../../types'
import { DetailsDialog } from '../dialogs/details-dialog'

const i18nKeys = {
  'Log Details': 'Log Details',
  Consume: 'Consume',
  'Billing Details': 'Billing Details',
  'Billing Mode': 'Billing Mode',
  'Per-token': 'Per-token',
  'Dynamic Pricing': 'Dynamic Pricing',
  'Matched Tier': 'Matched Tier',
  'Group Ratio': 'Group Ratio',
  'Total Cost': 'Total Cost',
  'Usage parameters': 'Usage parameters',
  'Upstream billing tokens': 'Upstream billing tokens',
  'Video Parameters': 'Video Parameters',
  'Requested Duration': 'Requested Duration',
  'Video Duration': 'Video Duration',
  Resolution: 'Resolution',
  'Aspect Ratio': 'Aspect Ratio',
  'Generation Time': 'Generation Time',
  'Token Breakdown': 'Token Breakdown',
  'Input Tokens': 'Input Tokens',
  'Output Tokens': 'Output Tokens',
  seconds: 'seconds',
}

function makeLog(other: LogOtherData): UsageLog {
  return {
    id: 1,
    user_id: 1,
    created_at: 1,
    type: 2,
    content: '',
    username: 'user',
    token_name: 'token',
    model_name: 'wan2.5-i2v-preview',
    quota: 5000,
    prompt_tokens: 0,
    completion_tokens: 0,
    use_time: 0,
    is_stream: false,
    channel: 1,
    channel_name: '',
    token_id: 1,
    group: 'default',
    ip: '',
    other: JSON.stringify(other),
    request_id: 'req-1',
    upstream_request_id: '',
  }
}

function renderDetails(other: LogOtherData, promptTokens = 0): QueryClient {
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
        log={{ ...makeLog(other), prompt_tokens: promptTokens }}
        isAdmin={false}
        isRoot={false}
        open
        onOpenChange={() => undefined}
      />
    </QueryClientProvider>
  )
  return queryClient
}

function rowValue(label: string): string | null {
  return screen.getByText(label).nextElementSibling?.textContent ?? null
}

test('shows the recorded request and response models in log details', () => {
  const queryClient = renderDetails({
    response_model: {
      requested_model: 'requested-model',
      upstream_model: 'mapped-model',
      returned_model: 'unexpected-model',
    },
  })
  expect(screen.getByText('Response model: unexpected-model')).toBeVisible()
  expect(rowValue('Request Model')).toBe('requested-model')
  expect(rowValue('Upstream Model')).toBe('mapped-model')
  expect(screen.getByText('unexpected-model')).toBeVisible()
  queryClient.clear()
})

describe('usage facts billing details', () => {
  test('shows the settled image count and a per-image price', () => {
    const queryClient = renderDetails({
      billing_mode: 'tiered_expr',
      expr_b64: btoa('tier("image", fixed(0.04)) * image_count'),
      billing_unit: 'request',
      fixed_price: 0.04,
      image_count: 2,
      matched_tier: 'image',
    })
    expect(
      screen.getByText('Billable image count').parentElement
    ).toHaveTextContent('2')
    expect(screen.getAllByText(/\/image/).length).toBeGreaterThan(0)
    queryClient.clear()
  })
  const queryClients: QueryClient[] = []

  test('shows actual billable image and cache tokens while retaining the aggregate cache count', () => {
    queryClients.push(
      renderDetails(
        {
          billing_mode: 'tiered_expr',
          expr_b64: btoa(
            'tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)'
          ),
          matched_tier: 'standard',
          cache_tokens: 300,
          image_cache_tokens: 200,
          billing_tokens: { p: 300, cr: 100, img: 400, img_cr: 200, c: 100 },
        },
        1000
      )
    )
    const billable = within(
      screen.getByRole('group', { name: 'Billable token breakdown' })
    )
    expect(
      billable.getByText('Image Cache').nextElementSibling
    ).toHaveTextContent('200')
    expect(
      billable.getByText('Cache Read').nextElementSibling
    ).toHaveTextContent('100')
    expect(billable.getByText('Image In').nextElementSibling).toHaveTextContent(
      '400'
    )
    expect(
      screen.getByText('Input Tokens').nextElementSibling
    ).toHaveTextContent('1,000')
    expect(
      screen
        .getAllByText('Cache Read')
        .some((label) => label.nextElementSibling?.textContent === '300')
    ).toBe(true)
  })

  beforeAll(() => {
    i18next.addResourceBundle('en', 'translation', i18nKeys)
  })

  afterEach(() => {
    for (const queryClient of queryClients) {
      queryClient.clear()
    }
    queryClients.length = 0
  })

  test('renders one raw-key row per usage fact before total cost', () => {
    const expression = 'tier("720P", u("seconds") * 5)'
    queryClients.push(
      renderDetails({
        group_ratio: 1,
        billing_mode: 'tiered_expr',
        expr_b64: Buffer.from(expression, 'utf8').toString('base64'),
        matched_tier: '720P',
        usage_facts: {
          resolution: '720P',
          seconds: 5,
        },
      })
    )

    expect(screen.getByText('Usage parameters')).toBeInTheDocument()
    expect(rowValue('resolution')).toBe('720P')
    expect(rowValue('seconds')).toBe('5')
    expect(rowValue('Billing Mode')).toBe('Dynamic Pricing')
    expect(rowValue('Matched Tier')).toBe('720P')

    const usageHeader = screen.getByText('Usage parameters')
    const totalCost = screen.getByText('Total Cost')
    expect(
      usageHeader.compareDocumentPosition(totalCost) &
        Node.DOCUMENT_POSITION_FOLLOWING
    ).toBeTruthy()
  })

  test('does not render usage parameter rows when usage_facts is absent', () => {
    queryClients.push(
      renderDetails({
        group_ratio: 1,
      })
    )

    expect(screen.queryByText('Usage parameters')).toBeNull()
    expect(screen.queryByText('resolution')).toBeNull()
    expect(screen.queryByText('seconds')).toBeNull()
    expect(screen.getByText('Total Cost')).toBeInTheDocument()
  })

  test('does not render usage parameter rows when usage_facts is empty', () => {
    queryClients.push(
      renderDetails({
        group_ratio: 1,
        usage_facts: {},
      })
    )

    expect(screen.queryByText('Usage parameters')).toBeNull()
    expect(screen.queryByText('resolution')).toBeNull()
    expect(screen.queryByText('seconds')).toBeNull()
    expect(screen.getByText('Total Cost')).toBeInTheDocument()
  })
})

describe('task log tokens and video parameters', () => {
  test('shows the upstream billing token total instead of an input/output pair', () => {
    const other: LogOtherData = {
      is_task: true,
      billing_mode: 'tiered_expr',
      expr_b64: btoa('tier("720p", u("tokens") * 0.00001)'),
      matched_tier: '720p',
      usage_facts: { tokens: 50638, resolution: '720p', video_input: 'none' },
    }
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
          log={{ ...makeLog(other), completion_tokens: 50638 }}
          isAdmin={false}
          isRoot={false}
          open
          onOpenChange={() => undefined}
        />
      </QueryClientProvider>
    )

    const label = screen.getByText('Upstream billing tokens')
    expect(label).toBeVisible()
    expect(label.nextElementSibling).toHaveTextContent('50,638')
    expect(screen.queryByText('Input Tokens')).toBeNull()
    expect(screen.queryByText('Output Tokens')).toBeNull()
    queryClient.clear()
  })

  test('shows the video parameters reported by the serving adaptor', () => {
    const other: LogOtherData = {
      is_task: true,
      video_requested_seconds: 5,
      video_seconds: 5,
      video_resolution: '720p',
      video_ratio: '16:9',
      video_generation_seconds: 42,
    }
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
          isAdmin={false}
          isRoot={false}
          open
          onOpenChange={() => undefined}
        />
      </QueryClientProvider>
    )

    expect(screen.getByText('Video Parameters')).toBeVisible()
    expect(rowValue('Requested Duration')).toBe('5 seconds')
    expect(rowValue('Video Duration')).toBe('5 seconds')
    expect(rowValue('Resolution')).toBe('720p')
    expect(rowValue('Aspect Ratio')).toBe('16:9')
    expect(rowValue('Generation Time')).not.toBeNull()
    queryClient.clear()
  })

  test('omits the video section when the log carries no video parameters', () => {
    const queryClient = renderDetails({ is_task: true })
    expect(screen.queryByText('Video Parameters')).toBeNull()
    queryClient.clear()
  })
})

describe('task refund settlement log', () => {
  test('shows the tiered breakdown for a task refund log', () => {
    const expression = 'tier("720p", u("tokens") * 0.00001)'
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
          log={{
            ...makeLog({
              is_task: true,
              billing_mode: 'tiered_expr',
              expr_b64: btoa(expression),
              matched_tier: '720p',
              usage_facts: { tokens: 1500, resolution: '720p' },
              task_id: 'task_abc',
            }),
            type: 6,
            completion_tokens: 1500,
            use_time: 42,
          }}
          isAdmin={false}
          isRoot={false}
          open
          onOpenChange={() => undefined}
        />
      </QueryClientProvider>
    )

    expect(screen.getByText('Refund Details')).toBeVisible()
    expect(screen.getAllByText('Dynamic Pricing').length).toBeGreaterThan(0)
    expect(rowValue('Matched Tier')).toBe('720p')
    queryClient.clear()
  })
})
