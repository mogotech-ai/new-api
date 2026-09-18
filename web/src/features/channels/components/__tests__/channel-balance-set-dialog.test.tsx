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
import type { Row } from '@tanstack/react-table'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { channelSchema, type Channel } from '../../types'
import { ChannelsDialogs } from '../channels-dialogs'
import { ChannelsProvider } from '../channels-provider'
import { DataTableRowActions } from '../data-table-row-actions'

const originalAuth = useAuthStore.getState().auth
let queryClient: QueryClient

beforeEach(() => {
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  useAuthStore.setState({
    auth: {
      ...originalAuth,
      user: { id: 1, username: 'root', role: ROLE.SUPER_ADMIN },
    },
  })
})

afterEach(() => {
  cleanup()
  queryClient.clear()
  vi.restoreAllMocks()
  useAuthStore.setState({ auth: originalAuth })
})

it('sets a channel balance from the row action', async () => {
  const channel = channelSchema.parse({
    id: 42,
    type: 1,
    key: '',
    name: 'Fallback channel',
    status: 1,
    created_time: 1,
    test_time: 0,
    response_time: 0,
    balance: 12.5,
    balance_updated_time: 0,
  })
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, balance: 98.75 },
  })
  const user = userEvent.setup()

  render(
    <QueryClientProvider client={queryClient}>
      <ChannelsProvider>
        <DataTableRowActions row={{ original: channel } as Row<Channel>} />
        <ChannelsDialogs />
      </ChannelsProvider>
    </QueryClientProvider>
  )

  await user.click(screen.getByRole('button', { name: 'Open menu' }))
  await user.click(screen.getByRole('menuitem', { name: 'Set Balance' }))

  const input = screen.getByRole('spinbutton', { name: 'Balance (USD)' })
  expect(input).toHaveValue(12.5)
  await user.clear(input)
  await user.type(input, '98.75')
  await user.click(screen.getByRole('button', { name: 'Save' }))

  await waitFor(() => {
    expect(post).toHaveBeenCalledWith(
      '/api/channel/update_balance/42',
      { balance: 98.75 },
      expect.any(Object)
    )
  })
  await waitFor(() => {
    expect(screen.queryByRole('dialog', { name: 'Set Balance' })).toBeNull()
  })
})

it('rejects a negative balance before sending a request', async () => {
  const channel = channelSchema.parse({
    id: 42,
    type: 1,
    key: '',
    name: 'Fallback channel',
    status: 1,
    created_time: 1,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
  })
  const post = vi.spyOn(api, 'post')
  const user = userEvent.setup()

  render(
    <QueryClientProvider client={queryClient}>
      <ChannelsProvider>
        <DataTableRowActions row={{ original: channel } as Row<Channel>} />
        <ChannelsDialogs />
      </ChannelsProvider>
    </QueryClientProvider>
  )

  await user.click(screen.getByRole('button', { name: 'Open menu' }))
  await user.click(screen.getByRole('menuitem', { name: 'Set Balance' }))
  const input = screen.getByRole('spinbutton', { name: 'Balance (USD)' })
  await user.clear(input)
  await user.type(input, '-1')
  await user.click(screen.getByRole('button', { name: 'Save' }))

  expect(input).toBeInvalid()
  expect(post).not.toHaveBeenCalled()
})
