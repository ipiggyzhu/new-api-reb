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
import { ArrowRight } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { AnimateInView } from '@/components/animate-in-view'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

interface CTAProps {
  className?: string
  isAuthenticated?: boolean
}

export function CTA(props: CTAProps) {
  const { t } = useTranslation()
  const baseUrl = `${window.location.origin}/v1`

  return (
    <section
      className={cn(
        'border-border/60 relative z-10 border-t px-5 py-20 sm:px-8 md:py-28',
        props.className
      )}
    >
      <AnimateInView className='mx-auto flex max-w-3xl flex-col items-center text-center'>
        <p className='section-eyebrow mb-6 justify-center'>
          {t('Three minutes to first request')}
        </p>
        <h2 className='font-heading text-3xl leading-[1.08] font-semibold tracking-tight text-balance md:text-5xl'>
          {t('Point your client at one base URL.')}
        </h2>
        <p className='text-muted-foreground mt-5 max-w-lg text-sm leading-7'>
          {t(
            'Deploy your own gateway and start routing requests through your configured upstream services.'
          )}
        </p>

        <dl className='bg-card/70 border-border/70 mt-9 w-full max-w-xl overflow-hidden rounded-xl border text-left font-mono text-xs'>
          <div className='border-border/60 flex flex-col gap-1 border-b px-4 py-3 sm:flex-row sm:items-center sm:gap-6'>
            <dt className='text-muted-foreground shrink-0 sm:w-28'>base_url</dt>
            <dd className='text-foreground truncate'>{baseUrl}</dd>
          </div>
          <div className='flex flex-col gap-1 px-4 py-3 sm:flex-row sm:items-center sm:gap-6'>
            <dt className='text-muted-foreground shrink-0 sm:w-28'>
              Authorization
            </dt>
            <dd className='text-foreground truncate'>
              Bearer sk-<span className='text-primary'>••••••••••••••••</span>
            </dd>
          </div>
        </dl>

        <Button
          className='group mt-9 h-12 rounded-lg px-6 text-sm'
          render={
            <Link to={props.isAuthenticated ? '/dashboard' : '/sign-up'} />
          }
        >
          {props.isAuthenticated ? t('Go to Dashboard') : t('Get Started')}
          <ArrowRight
            className='ml-1.5 size-4 transition-transform group-hover:translate-x-1 motion-reduce:transform-none'
            aria-hidden='true'
          />
        </Button>
      </AnimateInView>
    </section>
  )
}
