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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Loader2 } from 'lucide-react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { handleServerError } from '@/lib/handle-server-error'
import { createServerError } from '@/lib/server-error-message'

import { setChannelBalance } from '../../api'
import { channelsQueryKeys } from '../../lib'
import { useChannels } from '../channels-provider'

const balanceError = 'Balance must be a finite, non-negative number.'
const channelBalanceFormSchema = z.object({
  balance: z
    .number({ error: balanceError })
    .finite(balanceError)
    .min(0, balanceError),
})
type ChannelBalanceFormValues = z.infer<typeof channelBalanceFormSchema>

type ChannelBalanceSetDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function ChannelBalanceSetDialog(props: ChannelBalanceSetDialogProps) {
  const { t } = useTranslation()
  const { currentRow, setCurrentRow } = useChannels()
  const queryClient = useQueryClient()
  const form = useForm<ChannelBalanceFormValues>({
    resolver: zodResolver(channelBalanceFormSchema),
    defaultValues: { balance: currentRow?.balance ?? 0 },
  })
  const mutation = useMutation({
    mutationFn: async (values: ChannelBalanceFormValues) => {
      if (!currentRow) throw new Error('channel is required')
      const response = await setChannelBalance(currentRow.id, values.balance)
      if (!response.success || response.balance === undefined) {
        throw createServerError(response, t('Failed to update balance'))
      }
      return response.balance
    },
    onSuccess: async (balance) => {
      if (currentRow) {
        setCurrentRow({
          ...currentRow,
          balance,
          balance_updated_time: Math.floor(Date.now() / 1000),
        })
      }
      toast.success(t('Balance updated successfully'))
      props.onOpenChange(false)
      await queryClient.invalidateQueries({
        queryKey: channelsQueryKeys.lists(),
      })
    },
    onError: (error: unknown) => {
      handleServerError(error, t('Failed to update balance'))
    },
  })

  if (!currentRow) return null

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Set Balance')}
      description={
        <span className='space-y-1'>
          <span className='block'>
            {t('Update balance for:')}
            <strong>{currentRow.name}</strong>
          </span>
          <span className='text-muted-foreground block text-xs'>
            {t(
              'Enables a local spend limit. Settled usage deducts this balance and Auto-disable channel disables it at zero.'
            )}
          </span>
        </span>
      }
      contentHeight='auto'
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            onClick={() => props.onOpenChange(false)}
            disabled={mutation.isPending}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='submit'
            form='channel-balance-set-form'
            disabled={mutation.isPending}
          >
            {mutation.isPending && (
              <Loader2 className='mr-2 size-4 animate-spin' />
            )}
            {mutation.isPending ? t('Saving...') : t('Save')}
          </Button>
        </>
      }
    >
      <Form {...form}>
        <form
          id='channel-balance-set-form'
          className='py-4'
          onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
        >
          <FormField
            control={form.control}
            name='balance'
            render={({ field }) => (
              <FormItem>
                <FormLabel>
                  {t('Balance')} ({t('USD')})
                </FormLabel>
                <FormControl>
                  <Input
                    type='number'
                    min={0}
                    step='any'
                    disabled={mutation.isPending}
                    value={Number.isFinite(field.value) ? field.value : ''}
                    onBlur={field.onBlur}
                    onChange={(event) =>
                      field.onChange(
                        event.target.value === ''
                          ? Number.NaN
                          : event.target.valueAsNumber
                      )
                    }
                    ref={field.ref}
                    name={field.name}
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        </form>
      </Form>
    </Dialog>
  )
}
