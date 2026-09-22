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
import { useLocation } from '@tanstack/react-router'
import {
  Children,
  isValidElement,
  useState,
  type ReactElement,
  type ReactNode,
} from 'react'

import { useSidebarView } from '@/hooks/use-sidebar-view'

import { checkIsActive } from '../lib/url-utils'
import { Main } from './main'
import { PageFooterProvider } from './page-footer'

type SlotProps = { children?: ReactNode }

function SectionPageLayoutTitle(_props: SlotProps) {
  return null
}
SectionPageLayoutTitle.displayName = 'SectionPageLayout.Title'

function SectionPageLayoutActions(_props: SlotProps) {
  return null
}
SectionPageLayoutActions.displayName = 'SectionPageLayout.Actions'

function SectionPageLayoutContent(_props: SlotProps) {
  return null
}
SectionPageLayoutContent.displayName = 'SectionPageLayout.Content'

function SectionPageLayoutBreadcrumb(_props: SlotProps) {
  return null
}
SectionPageLayoutBreadcrumb.displayName = 'SectionPageLayout.Breadcrumb'

export type SectionPageLayoutProps = {
  children: ReactNode
  fixedContent?: boolean
}

/**
 * Page frame with a masthead: an eyebrow naming the owning nav section
 * (or the page's own breadcrumb), a display-face title, actions on the
 * trailing edge, and a gold hairline separating it from the content.
 */
export function SectionPageLayout(props: SectionPageLayoutProps) {
  const [footerContainer, setFooterContainer] = useState<HTMLDivElement | null>(
    null
  )
  const { view, navGroups } = useSidebarView()
  const href = useLocation({ select: (location) => location.href })

  let sectionTitle: string | undefined
  if (view) {
    sectionTitle = navGroups[0]?.title
  } else {
    sectionTitle = navGroups.find((group) =>
      group.items.some((item) => checkIsActive(href, item, true))
    )?.title
  }

  let title: ReactNode = null
  let actions: ReactNode = null
  let content: ReactNode = null
  let breadcrumb: ReactNode = null

  Children.forEach(props.children, (node) => {
    if (!isValidElement(node)) return
    const child = node as ReactElement<SlotProps>
    if (child.type === SectionPageLayoutTitle) {
      title = child.props.children
    } else if (child.type === SectionPageLayoutActions) {
      actions = child.props.children
    } else if (child.type === SectionPageLayoutContent) {
      content = child.props.children
    } else if (child.type === SectionPageLayoutBreadcrumb) {
      breadcrumb = child.props.children
    }
  })

  return (
    <PageFooterProvider container={footerContainer}>
      <Main>
        <header
          data-slot='masthead'
          className='shrink-0 px-4 pt-4 sm:px-7 sm:pt-6'
        >
          <div className='flex flex-wrap items-end justify-between gap-x-6 gap-y-3'>
            <div className='min-w-0 flex-1'>
              {breadcrumb != null ? (
                <div className='mb-1.5'>{breadcrumb}</div>
              ) : (
                sectionTitle && (
                  <p className='masthead-eyebrow mb-1.5'>
                    <span>{sectionTitle}</span>
                  </p>
                )
              )}
              <h1 className='font-heading truncate text-2xl leading-tight font-semibold tracking-tight sm:text-[1.75rem]'>
                {title}
              </h1>
            </div>
            {actions != null && (
              <div className='flex shrink-0 flex-wrap items-center justify-end gap-2'>
                {actions}
              </div>
            )}
          </div>
          <hr className='gold-hairline mt-4 sm:mt-5' />
        </header>

        <div
          className={
            props.fixedContent
              ? 'min-h-0 flex-1 overflow-hidden px-4 pt-4 pb-4 sm:px-7 sm:pt-5 sm:pb-6'
              : 'min-h-0 flex-1 overflow-auto px-4 pt-4 pb-4 sm:px-7 sm:pt-5 sm:pb-6'
          }
        >
          {content}
        </div>

        <div
          ref={setFooterContainer}
          className='bg-background/80 shrink-0 border-t px-4 py-2.5 backdrop-blur empty:hidden sm:px-7 sm:py-3'
        />
      </Main>
    </PageFooterProvider>
  )
}

SectionPageLayout.Title = SectionPageLayoutTitle
SectionPageLayout.Actions = SectionPageLayoutActions
SectionPageLayout.Content = SectionPageLayoutContent
SectionPageLayout.Breadcrumb = SectionPageLayoutBreadcrumb
