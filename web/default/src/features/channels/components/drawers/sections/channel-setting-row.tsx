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
import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

import {
  getInactiveSettingNotice,
  type ChannelSettingNotice,
  type ChannelSettingView,
} from '../../../lib/channel-setting-visibility'

type ChannelSettingRowProps = {
  setting: ChannelSettingView
  /** Why the option does nothing on this channel type. */
  reason?: string
  /**
   * Replaces the standard badge and sentence, for an option whose notice says
   * something other than "not used".
   */
  notice?: ChannelSettingNotice
  /**
   * `row`: the control brings its own row padding, as in the divided lists.
   * `block`: it does not, so the notice pads it.
   */
  layout?: 'row' | 'block'
  /** Extra classes for the notice, e.g. to span the columns of a grid. */
  className?: string
  children: ReactNode
}

/**
 * Shows one option according to its state: as it is when it applies, not at
 * all when it is hidden, and inside an amber notice when it is set on a
 * channel type that does not use it, so the admin sees it and knows what
 * happens to it.
 */
export function ChannelSettingRow(props: ChannelSettingRowProps) {
  const { t } = useTranslation()

  if (props.setting.state === 'hidden') return null
  if (props.setting.state === 'applicable') return props.children

  const block = props.layout === 'block'
  const notice =
    props.notice ?? getInactiveSettingNotice(props.setting, props.reason, t)
  return (
    <div className={cn(!block && 'p-2', props.className)}>
      <div
        className={cn(
          'rounded-lg border border-amber-200 bg-amber-50 text-amber-900 dark:border-amber-500/40 dark:bg-amber-500/10 dark:text-amber-50',
          block && 'space-y-4 p-4'
        )}
      >
        <div
          className={cn(
            'flex flex-wrap items-center gap-x-2 gap-y-1',
            !block && 'px-4 pt-3'
          )}
        >
          <Badge
            variant='outline'
            className='border-amber-300 bg-amber-100 text-amber-900 dark:border-amber-500/40 dark:bg-amber-500/20 dark:text-amber-50'
          >
            {notice.badge}
          </Badge>
          <p className='text-xs'>{notice.text}</p>
        </div>
        {props.children}
      </div>
    </div>
  )
}
