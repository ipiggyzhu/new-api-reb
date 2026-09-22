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
import { ArrowLeft } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { LanguageSwitcher } from '@/components/language-switcher'
import { ThemeSwitch } from '@/components/theme-switch'
import { Skeleton } from '@/components/ui/skeleton'
import { useSystemConfig } from '@/hooks/use-system-config'

type AuthLayoutProps = {
  children: React.ReactNode
}

function BrandLink() {
  const { systemName, logo, loading } = useSystemConfig()

  return (
    <Link
      to='/'
      className='inline-flex items-center gap-3 transition-opacity hover:opacity-80'
    >
      <span className='border-primary/30 bg-card flex size-9 items-center justify-center overflow-hidden rounded-lg border'>
        {loading ? (
          <Skeleton className='size-full' />
        ) : (
          <img src={logo} alt='' className='size-7 object-contain' />
        )}
      </span>
      {loading ? (
        <Skeleton className='h-5 w-24' />
      ) : (
        <span className='font-heading text-base font-semibold tracking-tight'>
          {systemName}
        </span>
      )}
    </Link>
  )
}

/**
 * Split-screen auth shell: a brand pane on the left (lg+) and the form
 * sitting directly on the canvas on the right. Below lg the brand pane is
 * dropped and the brand row moves into the header.
 */
export function AuthLayout(props: AuthLayoutProps) {
  const { t } = useTranslation()

  const facts = [
    {
      label: t('Providers'),
      value: t('Unified API for OpenAI, Claude, Gemini and 40+ providers'),
    },
    {
      label: t('Billing'),
      value: t('One key, one bill, one usage log'),
    },
    {
      label: t('Deployment'),
      value: t('Self-hosted, open, auditable'),
    },
  ]

  return (
    <div className='app-canvas flex min-h-svh flex-col lg:grid lg:grid-cols-2'>
      <aside className='border-border/70 hidden flex-col justify-between border-r px-12 py-10 lg:flex xl:px-16'>
        <BrandLink />

        <div className='py-16'>
          <h1 className='font-heading text-5xl leading-[1.05] font-semibold tracking-tight text-balance xl:text-6xl'>
            {t('The hub for')}
            <br />
            <span className='gold-text'>{t('every model.')}</span>
          </h1>
          <p className='text-muted-foreground mt-7 max-w-md text-sm leading-7'>
            {t(
              'Access a vast selection of models via a standard, unified API protocol. Power AI applications, manage digital assets, and connect the Future.'
            )}
          </p>
        </div>

        <dl className='max-w-md'>
          {facts.map((fact) => (
            <div
              key={fact.label}
              className='border-border/70 flex items-baseline justify-between gap-6 border-t py-3.5 last:border-b'
            >
              <dt className='text-muted-foreground shrink-0 font-mono text-[11px] tracking-[0.14em] uppercase'>
                {fact.label}
              </dt>
              <dd className='text-foreground/85 text-right text-xs leading-5'>
                {fact.value}
              </dd>
            </div>
          ))}
        </dl>
      </aside>

      <div className='flex min-h-svh flex-col lg:min-h-0'>
        <header className='flex items-center justify-between gap-4 px-5 py-5 sm:px-8'>
          <div className='lg:hidden'>
            <BrandLink />
          </div>
          <Link
            to='/'
            className='text-muted-foreground hover:text-primary hidden items-center gap-2 text-xs transition-colors lg:inline-flex'
          >
            <ArrowLeft className='size-3.5' aria-hidden='true' />
            {t('Home')}
          </Link>
          <div className='flex items-center gap-1'>
            <ThemeSwitch />
            <LanguageSwitcher />
          </div>
        </header>

        <main className='flex flex-1 items-start justify-center px-5 pt-8 pb-14 sm:px-8 lg:items-center lg:pt-6'>
          <div className='w-full max-w-[400px]'>{props.children}</div>
        </main>
      </div>
    </div>
  )
}
