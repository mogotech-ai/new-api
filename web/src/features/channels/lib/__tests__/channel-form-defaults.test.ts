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
import { describe, expect, test } from 'vitest'

import { channelSchema } from '../../types'
import { getChannelConfigurationState } from '../channel-configuration'
import {
  CHANNEL_FORM_DEFAULT_VALUES,
  channelFormSchema,
  buildSettingJSON,
  transformChannelToFormDefaults,
} from '../channel-form'

describe('channel logging defaults', () => {
  test('enables request and response logging for new channels', () => {
    expect(
      JSON.parse(buildSettingJSON(CHANNEL_FORM_DEFAULT_VALUES))
        .log_request_response_enabled
    ).toBe(true)
  })

  test('keeps logging disabled when editing a channel without the setting', () => {
    const channel = channelSchema.parse({
      id: 7,
      name: 'Existing channel',
      type: 1,
      key: '',
      status: 1,
      created_time: 1,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      models: 'gpt',
      group: 'default',
      setting: '{}',
    })
    expect(
      transformChannelToFormDefaults(channel).log_request_response_enabled
    ).toBe(false)
  })

  test('does not mark request processing configured for the new-channel default', () => {
    const state = (log: boolean, isEditing: boolean) =>
      getChannelConfigurationState(
        { ...CHANNEL_FORM_DEFAULT_VALUES, log_request_response_enabled: log },
        {},
        isEditing
      ).blocks.requestProcessing
    expect(state(true, false)).toBe('idle')
    expect(state(false, false)).toBe('configured')
    expect(state(true, true)).toBe('configured')
    expect(state(false, true)).toBe('idle')
  })
})

describe('OpenAI SIWC credentials', () => {
  const credential = {
    client_id: 'issued-client',
    access_token: 'access',
    refresh_token: 'refresh',
    expires_at: 1800000000,
  }
  const form = {
    ...CHANNEL_FORM_DEFAULT_VALUES,
    name: 'SIWC test',
    type: 1,
    models: 'model',
    group: ['default'],
  }

  test('accepts ordinary API keys and complete SIWC sessions in single-key mode', () => {
    expect(
      channelFormSchema.safeParse({ ...form, key: 'ordinary-api-key' }).success
    ).toBe(true)
    expect(
      channelFormSchema.safeParse({ ...form, key: JSON.stringify(credential) })
        .success
    ).toBe(true)
  })

  test('rejects incomplete sessions and the registration placeholder client ID', () => {
    expect(
      channelFormSchema.safeParse({
        ...form,
        key: JSON.stringify({
          client_id: '',
          access_token: '',
          refresh_token: '',
        }),
      }).success
    ).toBe(false)
    expect(
      channelFormSchema.safeParse({
        ...form,
        key: JSON.stringify({ ...credential, refresh_token: '' }),
      }).success
    ).toBe(false)
    expect(
      channelFormSchema.safeParse({
        ...form,
        key: JSON.stringify({
          ...credential,
          client_id: 'dynamic_agent_client',
        }),
      }).success
    ).toBe(false)
  })

  test('rejects treating a multiline OAuth session as a batch of API keys', () => {
    expect(
      channelFormSchema.safeParse({
        ...form,
        key: JSON.stringify(credential, null, 2),
        multi_key_mode: 'batch',
      }).success
    ).toBe(false)
  })
})
