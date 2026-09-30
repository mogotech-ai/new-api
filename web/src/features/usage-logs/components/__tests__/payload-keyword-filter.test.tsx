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
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { getCoreRowModel, useReactTable } from '@tanstack/react-table'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { buildApiParams } from '../../lib/utils'
import { CommonLogsFilterBar } from '../common-logs-filter-bar'
import { UsageLogsProvider } from '../usage-logs-provider'

function FilterFixture() {
  const table = useReactTable({
    data: [],
    columns: [],
    getCoreRowModel: getCoreRowModel(),
  })
  return (
    <UsageLogsProvider>
      <CommonLogsFilterBar table={table} />
    </UsageLogsProvider>
  )
}

async function renderFilter(role: number) {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'viewer', role })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: [] },
  })
  const root = createRootRoute()
  const auth = createRoute({ getParentRoute: () => root, id: '_authenticated' })
  const logs = createRoute({
    getParentRoute: () => auth,
    path: '/usage-logs/$section',
    component: FilterFixture,
    validateSearch: (search: Record<string, unknown>) => search,
  })
  const router = createRouter({
    routeTree: root.addChildren([auth.addChildren([logs])]),
    history: createMemoryHistory({ initialEntries: ['/usage-logs/common'] }),
  })
  render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
  await userEvent.click(await screen.findByRole('button', { name: 'Expand' }))
  return router
}

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.setUser(null)
})

it('submits a root content search with its scope and sends it to the log API', async () => {
  const router = await renderFilter(ROLE.SUPER_ADMIN)
  await userEvent.type(
    screen.getByRole('textbox', { name: 'Search request/response content' }),
    'refund'
  )
  await userEvent.click(
    screen.getByRole('combobox', { name: 'Content search scope' })
  )
  await userEvent.click(await screen.findByRole('option', { name: 'Response' }))
  await userEvent.click(screen.getByRole('button', { name: 'Search' }))
  await waitFor(() =>
    expect(router.state.location.search).toMatchObject({
      keyword: 'refund',
      keywordScope: 'response',
    })
  )
  expect(
    buildApiParams({
      page: 1,
      pageSize: 10,
      searchParams: router.state.location.search,
      isAdmin: true,
    })
  ).toMatchObject({ keyword: 'refund', keyword_scope: 'response' })
})

it('hides content search from administrators below root', async () => {
  await renderFilter(ROLE.ADMIN)
  expect(screen.getByPlaceholderText('Request ID')).toBeInTheDocument()
  expect(
    screen.queryByRole('textbox', { name: 'Search request/response content' })
  ).not.toBeInTheDocument()
})
