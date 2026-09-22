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
import { AnimatedOutlet } from '@/components/page-transition'
import { SkipToMain } from '@/components/skip-to-main'
import { SidebarInset, SidebarProvider } from '@/components/ui/sidebar'
import { LayoutProvider } from '@/context/layout-provider'
import { SearchProvider } from '@/context/search-provider'
import { getCookie } from '@/lib/cookies'

import { AppSidebar } from './app-sidebar'
import { MobileTopBar } from './mobile-top-bar'

type AuthenticatedLayoutProps = {
  children?: React.ReactNode
}

/**
 * Authenticated shell: one sidebar (brand, search, nav groups, site links,
 * global controls) beside the page. There is no global top bar; page
 * context lives in each page's masthead via `SectionPageLayout`. Below
 * `md` a compact top bar carries the sheet trigger.
 */
export function AuthenticatedLayout(props: AuthenticatedLayoutProps) {
  const defaultOpen = getCookie('sidebar_state') !== 'false'

  return (
    <LayoutProvider>
      <SearchProvider>
        <SidebarProvider
          defaultOpen={defaultOpen}
          className='app-canvas flex-col md:flex-row'
          style={
            {
              '--sidebar-width': '14rem',
              // No global top bar in this shell: the fixed sidebar starts
              // at the viewport top instead of below the header slot.
              '--app-header-height': '0px',
            } as React.CSSProperties
          }
        >
          <SkipToMain />
          <MobileTopBar />
          <AppSidebar />
          <SidebarInset
            id='content'
            className='@container/content h-[calc(100svh-3rem)] min-h-0 min-w-0 overflow-hidden md:h-svh md:peer-data-[variant=inset]:h-[calc(100svh-(var(--spacing)*4))]'
          >
            {props.children ?? <AnimatedOutlet />}
          </SidebarInset>
        </SidebarProvider>
      </SearchProvider>
    </LayoutProvider>
  )
}
