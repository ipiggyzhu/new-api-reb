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
import { Link } from '@tanstack/react-router'
import { ChevronsUpDown, Compass, ExternalLink, SearchIcon } from 'lucide-react'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { useTranslation } from 'react-i18next'

import { ConfigDrawer } from '@/components/config-drawer'
import { LanguageSwitcher } from '@/components/language-switcher'
import { NotificationPopover } from '@/components/notification-popover'
import { ProfileDropdown } from '@/components/profile-dropdown'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
  SidebarTrigger,
  useSidebar,
} from '@/components/ui/sidebar'
import { useLayout } from '@/context/layout-provider'
import { useSearch } from '@/context/search-provider'
import { useNotifications } from '@/hooks/use-notifications'
import { useSidebarView } from '@/hooks/use-sidebar-view'
import { useTopNavLinks } from '@/hooks/use-top-nav-links'
import { MOTION_TRANSITION, MOTION_VARIANTS } from '@/lib/motion'

import { NavGroup } from './nav-group'
import { SidebarViewHeader } from './sidebar-view-header'
import { SystemBrand } from './system-brand'

/**
 * Application sidebar: brand and search on top, every nav group in the
 * middle (or the drill-in workspace registered for the URL), and the
 * site links plus global controls pinned to the bottom. Collapses to an
 * icon rail (⌘B) where each entry keeps a tooltip.
 */
export function AppSidebar() {
  const { t } = useTranslation()
  const { collapsible, variant } = useLayout()
  const { key, view, navGroups } = useSidebarView()
  const { setOpen: setSearchOpen } = useSearch()
  const { isMobile, setOpenMobile } = useSidebar()
  const notifications = useNotifications()
  const siteLinks = useTopNavLinks()
  const shouldReduce = useReducedMotion()

  return (
    <Sidebar collapsible={collapsible} variant={variant}>
      <SidebarHeader className='gap-1.5 px-2 pt-2.5 pb-1.5'>
        <div className='flex items-center gap-1 group-data-[collapsible=icon]:flex-col'>
          <div className='min-w-0 flex-1'>
            <SystemBrand variant='sidebar' />
          </div>
          <SidebarTrigger className='text-muted-foreground size-7 shrink-0' />
        </div>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              tooltip={t('Search')}
              className='sidebar-search text-muted-foreground'
              onClick={() => setSearchOpen(true)}
            >
              <SearchIcon className='shrink-0' />
              <span className='min-w-0 flex-1 truncate'>{t('Search')}</span>
              <kbd className='sidebar-kbd'>⌘K</kbd>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>

      {view && <SidebarViewHeader view={view} />}

      <SidebarContent className='py-1'>
        <AnimatePresence mode='wait' initial={false}>
          <motion.div
            key={key}
            initial={
              shouldReduce ? false : MOTION_VARIANTS.sidebarSlide.initial
            }
            animate={MOTION_VARIANTS.sidebarSlide.animate}
            exit={shouldReduce ? undefined : MOTION_VARIANTS.sidebarSlide.exit}
            transition={MOTION_TRANSITION.fast}
            className='flex flex-col'
          >
            {navGroups.map((group) => (
              <NavGroup key={group.id || group.title} {...group} />
            ))}
          </motion.div>
        </AnimatePresence>
      </SidebarContent>

      <SidebarFooter className='border-sidebar-border gap-1 border-t px-2 py-2'>
        {siteLinks.length > 0 && (
          <SidebarMenu>
            <SidebarMenuItem>
              <DropdownMenu>
                <DropdownMenuTrigger
                  render={
                    <SidebarMenuButton
                      tooltip={t('Site links')}
                      className='text-muted-foreground'
                    />
                  }
                >
                  <Compass className='shrink-0' />
                  <span className='min-w-0 flex-1 truncate'>
                    {t('Site links')}
                  </span>
                  <ChevronsUpDown className='size-3.5 shrink-0 opacity-60' />
                </DropdownMenuTrigger>
                <DropdownMenuContent
                  side={isMobile ? 'top' : 'right'}
                  align='end'
                  className='min-w-44'
                >
                  {siteLinks.map((link) => (
                    <DropdownMenuItem
                      key={link.href}
                      disabled={link.disabled}
                      render={
                        link.external ? (
                          <a
                            href={link.href}
                            target='_blank'
                            rel='noopener noreferrer'
                          />
                        ) : (
                          <Link
                            to={link.href}
                            onClick={() => setOpenMobile(false)}
                          />
                        )
                      }
                    >
                      <span className='flex-1'>{link.title}</span>
                      {link.external && (
                        <ExternalLink
                          className='text-muted-foreground size-3.5'
                          aria-hidden='true'
                        />
                      )}
                    </DropdownMenuItem>
                  ))}
                </DropdownMenuContent>
              </DropdownMenu>
            </SidebarMenuItem>
          </SidebarMenu>
        )}
        <div className='flex items-center gap-0.5 group-data-[collapsible=icon]:flex-col'>
          <div className='flex size-8 shrink-0 items-center justify-center'>
            <ProfileDropdown />
          </div>
          <span className='flex-1 group-data-[collapsible=icon]:hidden' />
          <NotificationPopover
            open={notifications.popoverOpen}
            onOpenChange={notifications.setPopoverOpen}
            unreadCount={notifications.unreadCount}
            activeTab={notifications.activeTab}
            onTabChange={notifications.setActiveTab}
            notice={notifications.notice}
            announcements={notifications.announcements}
            loading={notifications.loading}
            className='size-8'
          />
          <LanguageSwitcher />
          <ConfigDrawer />
        </div>
      </SidebarFooter>

      <SidebarRail />
    </Sidebar>
  )
}
