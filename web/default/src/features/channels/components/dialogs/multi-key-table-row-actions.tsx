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
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'

import type { MultiKeyConfirmAction } from '../../types'

type MultiKeyTableRowActionsProps = {
  keyIndex: number
  status: number
  canEditSensitive: boolean
  onAction: (action: MultiKeyConfirmAction) => void
  onEditEnvironment: () => void
}

export function MultiKeyTableRowActions({
  keyIndex,
  status,
  canEditSensitive,
  onAction,
  onEditEnvironment,
}: MultiKeyTableRowActionsProps) {
  const { t } = useTranslation()
  const isEnabled = status === 1
  const noPermissionTitle = canEditSensitive
    ? undefined
    : t('No permission to perform this action')

  return (
    <div className='flex justify-end gap-2'>
      {isEnabled ? (
        <Button
          variant='outline'
          size='sm'
          onClick={() => onAction({ type: 'disable', keyIndex })}
        >
          {t('Disable')}
        </Button>
      ) : (
        <Button
          variant='outline'
          size='sm'
          onClick={() => onAction({ type: 'enable', keyIndex })}
        >
          {t('Enable')}
        </Button>
      )}
      <Button
        variant='outline'
        size='sm'
        onClick={() => {
          if (!canEditSensitive) return
          onEditEnvironment()
        }}
        disabled={!canEditSensitive}
        title={noPermissionTitle}
      >
        {t('Edit Environment')}
      </Button>
      <Button
        variant='destructive'
        size='sm'
        onClick={() => {
          if (!canEditSensitive) return
          onAction({ type: 'delete', keyIndex })
        }}
        disabled={!canEditSensitive}
        title={noPermissionTitle}
      >
        {t('Delete')}
      </Button>
    </div>
  )
}
