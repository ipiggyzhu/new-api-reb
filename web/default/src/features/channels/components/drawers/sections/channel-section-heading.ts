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
import { createElement, type ReactNode } from 'react'

import { SideDrawerSectionHeader } from '@/components/drawer-layout'
import { IconBadge, type IconBadgeTone } from '@/components/ui/icon-badge'
import { cn } from '@/lib/utils'

// Headings and the card outline shared by the channel editor drawer and its
// advanced-area cards. Written with createElement, like drawer-layout.ts, so
// the class name helper can live in the same module as the two headings.

/**
 * Heading of a card. With a description it is the header the other sections
 * of the drawer use: the title with one muted line under it.
 */
export function CardHeading(props: {
  title: string
  description?: string
  icon?: ReactNode
  iconTone?: IconBadgeTone
}) {
  if (props.description) {
    return createElement(SideDrawerSectionHeader, {
      title: props.title,
      description: props.description,
      icon: props.icon,
      iconTone: props.iconTone,
    })
  }

  return createElement(
    'div',
    { className: 'flex items-center gap-3' },
    props.icon
      ? createElement(
          IconBadge,
          { tone: props.iconTone, size: 'md' },
          props.icon
        )
      : null,
    createElement(
      'h3',
      { className: 'text-sm font-semibold tracking-tight' },
      props.title
    )
  )
}

export function SubHeading(props: {
  title: string
  icon?: ReactNode
  iconTone?: IconBadgeTone
}) {
  return createElement(
    'div',
    { className: 'flex items-center gap-2' },
    props.icon
      ? createElement(
          IconBadge,
          { tone: props.iconTone, size: 'xs' },
          props.icon
        )
      : null,
    createElement(
      'h4',
      {
        className:
          'text-muted-foreground text-xs font-medium tracking-wide uppercase',
      },
      props.title
    )
  )
}

export function configuredAdvancedSectionClassName(
  className: string,
  configured: boolean
) {
  return cn(
    className,
    'border-border/60 rounded-lg border p-3 transition-colors',
    configured && 'border-primary/35 ring-primary/20 ring-1'
  )
}
