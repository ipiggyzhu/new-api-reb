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

import { sideDrawerSectionClassName } from '@/components/drawer-layout'
import type { IconBadgeTone } from '@/components/ui/icon-badge'

import {
  CardHeading,
  configuredAdvancedSectionClassName,
} from './channel-section-heading'

type ChannelSettingsCardProps = {
  id: string
  title: string
  description: string
  icon: ReactNode
  iconTone: IconBadgeTone
  /** Something on the card is set, so it gets the highlighted outline. */
  configured: boolean
  sensitiveLocked: boolean
  children: ReactNode
}

/** One card of channel-level settings in the advanced area of the editor. */
export function ChannelSettingsCard(props: ChannelSettingsCardProps) {
  return (
    <div
      id={props.id}
      className={sideDrawerSectionClassName(
        configuredAdvancedSectionClassName(
          // The drawer section style drops the bottom border and padding of a
          // last child, which would leave the last card open at the bottom.
          'scroll-mt-4 last:border-b last:pb-3',
          props.configured
        )
      )}
    >
      <CardHeading
        title={props.title}
        description={props.description}
        icon={props.icon}
        iconTone={props.iconTone}
      />
      <fieldset
        disabled={props.sensitiveLocked}
        className='space-y-4 disabled:opacity-60'
      >
        {props.children}
      </fieldset>
    </div>
  )
}
