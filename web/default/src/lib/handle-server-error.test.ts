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
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { AxiosError } from 'axios'
import i18next from 'i18next'
import { toast } from 'sonner'

import { handleServerError } from './handle-server-error'

describe('server error notifications', () => {
  test('shows the API message and retains a translated fallback without response data', async () => {
    await i18next.init({
      lng: 'en',
      resources: {
        en: { translation: { 'Something went wrong!': 'Fallback message' } },
      },
    })
    for (const [responseData, expected] of [
      [
        {
          success: false,
          message: 'Channel configuration rejected',
          title: 'Legacy title',
        },
        'Channel configuration rejected',
      ],
      [{ title: 'Legacy title' }, 'Legacy title'],
      [{ success: false, message: '' }, 'Fallback message'],
      [undefined, 'Fallback message'],
    ] as const) {
      const error = new AxiosError('Request failed')
      if (responseData) {
        error.response = {
          data: responseData,
          status: 400,
          statusText: 'Bad Request',
          headers: {},
          config: { headers: {} } as NonNullable<typeof error.config>,
        }
      }
      handleServerError(error)
      const notification = toast.getHistory().at(-1)
      assert.ok(notification && 'title' in notification)
      assert.equal(notification.title, expected)
    }
  })
})
