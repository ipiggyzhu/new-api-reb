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

import { AnimateInView } from '@/components/animate-in-view'
import { cn } from '@/lib/utils'

interface CapabilitiesProps {
  className?: string
}

/**
 * Capabilities ledger: a numbered list because the gateway's feature set is
 * a fixed, enumerable inventory — not a grid of interchangeable cards.
 */
export function Capabilities(props: CapabilitiesProps) {
  const { t } = useTranslation()

  const rows = [
    {
      title: t('Unified protocol'),
      desc: t(
        'One standard endpoint in front of every upstream. Switch models without touching client code.'
      ),
      tag: t('Protocol'),
    },
    {
      title: t('Routing & failover'),
      desc: t(
        'Weighted channels, automatic retries and health-based demotion keep requests flowing.'
      ),
      tag: t('Routing'),
    },
    {
      title: t('Billing & quotas'),
      desc: t(
        'Per-model ratios, prepaid balances and group pricing, settled on every request.'
      ),
      tag: t('Billing'),
    },
    {
      title: t('Teams & permissions'),
      desc: t(
        'Users, groups and scoped keys with role-based access to the console.'
      ),
      tag: t('Access'),
    },
    {
      title: t('Logs & observability'),
      desc: t(
        'Every request recorded with tokens, latency and cost. Drill in from the dashboard.'
      ),
      tag: t('Insight'),
    },
    {
      title: t('Self-hosted & open'),
      desc: t(
        'Run it on your own infrastructure with SQLite, MySQL or PostgreSQL.'
      ),
      tag: t('Open Source'),
    },
  ]

  return (
    <section
      className={cn(
        'relative z-10 px-5 py-20 sm:px-8 md:py-28',
        props.className
      )}
    >
      <div className='mx-auto max-w-6xl'>
        <AnimateInView className='mb-12 max-w-xl'>
          <p className='section-eyebrow mb-5'>{t('Capabilities')}</p>
          <h2 className='font-heading text-3xl leading-[1.1] font-semibold tracking-tight md:text-[2.5rem]'>
            {t('One entry point, every capability.')}
          </h2>
        </AnimateInView>

        <ol className='border-border/60 border-t'>
          {rows.map((row, index) => (
            <AnimateInView
              key={row.title}
              as='li'
              delay={index * 60}
              className='group border-border/60 relative grid grid-cols-[2.5rem_minmax(0,1fr)] items-baseline gap-x-5 gap-y-2 border-b px-3 py-6 md:grid-cols-[4rem_minmax(0,15rem)_minmax(0,1fr)_auto] md:gap-x-8 md:px-4'
            >
              <span
                className='bg-primary absolute inset-y-4 left-0 w-0.5 origin-top scale-y-0 rounded-full transition-transform duration-300 group-hover:scale-y-100 motion-reduce:transition-none'
                aria-hidden='true'
              />
              <span className='text-primary/80 font-mono text-xs tabular-nums'>
                {String(index + 1).padStart(2, '0')}
              </span>
              <h3 className='text-base font-semibold tracking-tight'>
                {row.title}
              </h3>
              <p className='text-muted-foreground col-start-2 text-sm leading-relaxed md:col-start-3'>
                {row.desc}
              </p>
              <span className='text-muted-foreground border-border/70 col-start-2 justify-self-start rounded-full border px-2.5 py-0.5 font-mono text-[10px] tracking-wider uppercase md:col-start-4 md:self-center'>
                {row.tag}
              </span>
            </AnimateInView>
          ))}
        </ol>
      </div>
    </section>
  )
}
