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
import { useMutation } from '@tanstack/react-query'
import { ExternalLink, Loader2 } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { Button, buttonVariants } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { handleServerError } from '@/lib/handle-server-error'
import { createServerError } from '@/lib/server-error-message'

import { completeSIWCLogin, startSIWCLogin } from '../../api'

type SIWCLoginDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  onCredential: (key: string) => void
}

export function SIWCLoginDialog(props: SIWCLoginDialogProps) {
  const { t } = useTranslation()
  const [authorizeUrl, setAuthorizeUrl] = useState('')
  const [callbackUrl, setCallbackUrl] = useState('')

  const start = useMutation({
    mutationFn: async () => {
      const response = await startSIWCLogin()
      if (!response.success || !response.data?.authorize_url) {
        throw createServerError(response, t('Failed to start ChatGPT sign-in'))
      }
      return response.data.authorize_url
    },
    onSuccess: setAuthorizeUrl,
    onError: (error: unknown) => {
      handleServerError(error, t('Failed to start ChatGPT sign-in'))
    },
  })

  const complete = useMutation({
    mutationFn: async () => {
      const response = await completeSIWCLogin(callbackUrl.trim())
      if (!response.success || !response.data?.key) {
        throw createServerError(
          response,
          t('Failed to complete ChatGPT sign-in')
        )
      }
      return response.data.key
    },
    onSuccess: (key) => {
      props.onCredential(key)
      toast.success(t('ChatGPT sign-in completed'))
      setAuthorizeUrl('')
      setCallbackUrl('')
      props.onOpenChange(false)
    },
    onError: (error: unknown) => {
      handleServerError(error, t('Failed to complete ChatGPT sign-in'))
    },
  })

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Sign in with ChatGPT')}
      description={t(
        'Creates a SIWC credential for this channel. Requests use the signed-in ChatGPT plan.'
      )}
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            onClick={() => props.onOpenChange(false)}
            disabled={complete.isPending}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='button'
            onClick={() => complete.mutate()}
            disabled={!authorizeUrl || !callbackUrl.trim() || complete.isPending}
          >
            {complete.isPending && (
              <Loader2 className='mr-2 size-4 animate-spin' />
            )}
            {t('Complete sign-in')}
          </Button>
        </>
      }
    >
      <div className='flex flex-col gap-4'>
        <ol className='text-muted-foreground list-decimal space-y-1 pl-5 text-sm'>
          <li>{t('Open the ChatGPT sign-in page and approve access.')}</li>
          <li>
            {t(
              'The browser then lands on a 127.0.0.1 page that does not load. Copy the full URL from the address bar.'
            )}
          </li>
          <li>{t('Paste it below within 10 minutes.')}</li>
        </ol>
        <div className='flex flex-wrap gap-2'>
          <Button
            type='button'
            variant='outline'
            size='sm'
            onClick={() => start.mutate()}
            disabled={start.isPending}
          >
            {start.isPending && (
              <Loader2 className='mr-2 size-4 animate-spin' />
            )}
            {authorizeUrl
              ? t('Create a new sign-in link')
              : t('Create sign-in link')}
          </Button>
          {authorizeUrl && (
            <a
              href={authorizeUrl}
              target='_blank'
              rel='noopener noreferrer'
              className={buttonVariants({ size: 'sm' })}
            >
              <ExternalLink className='mr-2 size-4' aria-hidden='true' />
              {t('Open ChatGPT sign-in')}
            </a>
          )}
        </div>
        <div className='flex flex-col gap-2'>
          <Label htmlFor='siwc-callback-url'>{t('Callback URL')}</Label>
          <Input
            id='siwc-callback-url'
            value={callbackUrl}
            onChange={(event) => setCallbackUrl(event.target.value)}
            placeholder='http://127.0.0.1:1455/auth/callback?code=...'
            autoComplete='off'
            disabled={!authorizeUrl || complete.isPending}
          />
        </div>
      </div>
    </Dialog>
  )
}
