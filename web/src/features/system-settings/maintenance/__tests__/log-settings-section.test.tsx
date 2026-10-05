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
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { Toaster } from 'sonner'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { LogSettingsSection } from '../log-settings-section'

function Fixture(props: Partial<Parameters<typeof LogSettingsSection>[0]>) {
  const [container, setContainer] = useState<HTMLDivElement | null>(null)
  const [client] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: { retry: false },
          mutations: { retry: false },
        },
      })
  )
  return (
    <QueryClientProvider client={client}>
      <div ref={setContainer} />
      <SettingsPageProvider actionsContainer={container}>
        <LogSettingsSection
          defaultEnabled
          rawRequestDefaultEnabled={false}
          {...props}
        />
      </SettingsPageProvider>
      <Toaster />
    </QueryClientProvider>
  )
}

function renderSection(
  props: Partial<Parameters<typeof LogSettingsSection>[0]> = {}
) {
  return render(<Fixture {...props} />)
}

// 挂载时会拉一次服务器日志信息与当前清理任务，这两个接口与本次契约无关。
function mockMountReads() {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: null },
  })
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('log settings save', () => {
  // 2026-10-05 复现：onSubmit 逐个 key 顺序 await，第一个成功第二个失败时
  // 用户只看到一句笼统报错，不知道「第一项已经存上了」。
  it('one changed option failing reports the partial save explicitly', async () => {
    mockMountReads()
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValueOnce({ data: { success: true } })
      .mockRejectedValueOnce(new Error('db down'))
    const user = userEvent.setup()
    renderSection()

    // 两个开关都改掉 → 提交两项更新，第一项成功、第二项失败
    await user.click(screen.getByRole('switch', { name: 'Record quota usage' }))
    await user.click(
      screen.getByRole('switch', {
        name: 'Record raw request info in error logs',
      })
    )
    await user.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )

    expect(
      await screen.findByText(
        '1 of 2 settings were saved before the failure. Review the remaining settings and try again.'
      )
    ).toBeVisible()
    expect(put).toHaveBeenCalledTimes(2)
  })

  it('all options saving successfully shows only the per-key success feedback', async () => {
    mockMountReads()
    vi.spyOn(api, 'put').mockResolvedValue({ data: { success: true } })
    const user = userEvent.setup()
    renderSection()

    await user.click(screen.getByRole('switch', { name: 'Record quota usage' }))
    await user.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )

    await waitFor(() =>
      expect(
        screen.queryByText(/were saved before the failure/)
      ).not.toBeInTheDocument()
    )
    expect(
      screen.queryByText('Failed to update setting')
    ).not.toBeInTheDocument()
  })
})
