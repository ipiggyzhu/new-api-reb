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

import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

import { RouteBoard } from '../route-board'

interface HeroProps {
  className?: string
  isAuthenticated?: boolean
}

export function Hero(props: HeroProps) {
  const { t } = useTranslation()

  return (
    <section
      className={cn(
        'landing-hero relative z-10 px-5 pt-28 pb-16 sm:px-8 lg:pt-36 lg:pb-20',
        props.className
      )}
    >
      <div className='mx-auto grid max-w-6xl items-start gap-12 lg:grid-cols-[minmax(0,1.05fr)_minmax(0,0.95fr)] lg:gap-16'>
        <div className='landing-animate-fade-up min-w-0 lg:pt-8'>
          <p className='section-eyebrow'>
            {t('AI Application Infrastructure Foundation')}
          </p>

          <h1 className='font-heading mt-7 text-[clamp(2.75rem,6.2vw,5.25rem)] leading-[1.02] font-semibold tracking-[-0.04em] text-balance'>
            {t('One key,')}
            <br />
            <span className='text-primary'>{t('every model.')}</span>
          </h1>

          <p className='text-muted-foreground mt-7 max-w-md text-[15px] leading-8'>
            {t(
              'Access a vast selection of models via a standard, unified API protocol. Power AI applications, manage digital assets, and connect the Future.'
            )}
          </p>

          <div className='mt-9 flex flex-wrap items-center gap-3'>
            <Button
              className='group h-12 rounded-lg px-6 text-sm'
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
            <Button
              variant='outline'
              className='h-12 rounded-lg px-5 text-sm'
              render={<Link to='/pricing' />}
            >
              {t('View Pricing')}
            </Button>
          </div>

          <p className='text-muted-foreground/70 mt-12 font-mono text-[11px] tracking-[0.14em] uppercase'>
            {t('Compatible with OpenAI, Claude and Gemini protocols')}
          </p>
        </div>

        <div className='terminal-stage landing-animate-fade-up min-w-0'>
          <RouteBoard />
        </div>
      </div>
    </section>
  )
}
