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
import { SearchIcon } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { NotificationPopover } from '@/components/notification-popover'
import { ProfileDropdown } from '@/components/profile-dropdown'
import { Button } from '@/components/ui/button'
import { SidebarTrigger } from '@/components/ui/sidebar'
import { useSearch } from '@/context/search-provider'
import { useNotifications } from '@/hooks/use-notifications'

import { SystemBrand } from './system-brand'

/**
 * Compact top bar for viewports below `md`, where the rail and section
 * panel give way to a sheet. The trigger opens that sheet.
 */
export function MobileTopBar() {
  const { t } = useTranslation()
  const { setOpen: setSearchOpen } = useSearch()
  const notifications = useNotifications()

  return (
    <header
      data-slot='mobile-top-bar'
      className='bg-sidebar border-sidebar-border sticky top-0 z-40 flex h-12 shrink-0 items-center gap-1.5 border-b px-2 md:hidden'
    >
      <SidebarTrigger variant='ghost' className='size-8' />
      <SystemBrand variant='inline' />
      <div className='ms-auto flex items-center gap-0.5'>
        <Button
          variant='ghost'
          size='icon'
          className='size-8'
          aria-label={t('Search')}
          onClick={() => setSearchOpen(true)}
        >
          <SearchIcon className='size-4' aria-hidden='true' />
        </Button>
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
        <div className='flex size-8 items-center justify-center'>
          <ProfileDropdown />
        </div>
      </div>
    </header>
  )
}
