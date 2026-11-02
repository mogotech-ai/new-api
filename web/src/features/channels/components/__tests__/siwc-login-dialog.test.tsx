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
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { SIWCLoginDialog } from '../dialogs/siwc-login-dialog'

const authorizeUrl = 'https://auth.openai.com/api/accounts/authorize?state=s'
const callbackUrl =
  'http://127.0.0.1:1455/auth/callback?code=c&state=s&client_id=oaiapp_x'
let queryClient: QueryClient

beforeEach(() => {
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
})

afterEach(() => {
  cleanup()
  queryClient.clear()
  vi.restoreAllMocks()
})

function renderDialog(onCredential: (key: string) => void) {
  const onOpenChange = vi.fn()
  render(
    <QueryClientProvider client={queryClient}>
      <SIWCLoginDialog
        open
        onOpenChange={onOpenChange}
        onCredential={onCredential}
      />
    </QueryClientProvider>
  )
  return onOpenChange
}

async function startAndPaste(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: 'Create sign-in link' }))
  expect(
    await screen.findByRole('link', { name: 'Open ChatGPT sign-in' })
  ).toHaveAttribute('href', authorizeUrl)
  await user.type(screen.getByLabelText('Callback URL'), callbackUrl)
  await user.click(screen.getByRole('button', { name: 'Complete sign-in' }))
}

it('fills the channel key when the pasted callback is exchanged', async () => {
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: { success: true, data: { authorize_url: authorizeUrl } },
    })
    .mockResolvedValueOnce({
      data: { success: true, data: { key: '{"client_id":"oaiapp_x"}' } },
    })
  const onCredential = vi.fn()
  const onOpenChange = renderDialog(onCredential)

  await startAndPaste(userEvent.setup())

  await waitFor(() =>
    expect(onCredential).toHaveBeenCalledWith('{"client_id":"oaiapp_x"}')
  )
  expect(post).toHaveBeenLastCalledWith(
    '/api/channel/siwc/login/complete',
    { callback_url: callbackUrl },
    expect.anything()
  )
  expect(onOpenChange).toHaveBeenCalledWith(false)
})

it('leaves the channel key unchanged when the server rejects the callback', async () => {
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: { success: true, data: { authorize_url: authorizeUrl } },
    })
    .mockResolvedValueOnce({
      data: {
        success: false,
        message: 'SIWC sign-in expired or was already used; start again',
      },
    })
  const onCredential = vi.fn()
  const onOpenChange = renderDialog(onCredential)

  await startAndPaste(userEvent.setup())

  await waitFor(() => expect(post).toHaveBeenCalledTimes(2))
  expect(onCredential).not.toHaveBeenCalled()
  expect(onOpenChange).not.toHaveBeenCalledWith(false)
})
