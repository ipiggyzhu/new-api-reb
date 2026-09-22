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
import DOMPurify from 'dompurify'
import { Fragment, useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { useStatus } from '@/hooks/use-status'
import { useSystemConfig } from '@/hooks/use-system-config'
import { DEFAULT_LOGO, DEFAULT_SYSTEM_NAME } from '@/lib/constants'
import { cn } from '@/lib/utils'

interface FooterLink {
  text: string
  href: string
}

interface FooterColumnProps {
  title: string
  links: FooterLink[]
}

interface FooterProps {
  logo?: string
  name?: string
  columns?: FooterColumnProps[]
  copyright?: string
  className?: string
}

const NEW_API_FOOTER_ATTRIBUTION_KEY = [
  'footer',
  'new' + 'api',
  'projectAttributionSuffix',
].join('.')

function FooterLinkItem(props: { link: FooterLink }) {
  const { t } = useTranslation()
  const isExternal = props.link.href.startsWith('http')
  const label = t(props.link.text)

  if (isExternal) {
    return (
      <a
        href={props.link.href}
        target='_blank'
        rel='noopener noreferrer'
        className='text-muted-foreground hover:text-foreground text-sm transition-colors duration-200'
      >
        {label}
      </a>
    )
  }

  return (
    <Link
      to={props.link.href}
      className='text-muted-foreground hover:text-foreground text-sm transition-colors duration-200'
    >
      {label}
    </Link>
  )
}

// Renders User Agreement / Privacy Policy links inline with the parent's
// copyright row when either is configured in System Settings → Site. Emits
// fragmented siblings so the parent flex container's gap controls spacing.
function LegalLinks(props: { leadingSeparator?: boolean }) {
  const { t } = useTranslation()
  const { status } = useStatus()
  const items: { key: string; label: string; href: string }[] = []
  if (status?.user_agreement_enabled) {
    items.push({
      key: 'user-agreement',
      label: t('User Agreement'),
      href: '/user-agreement',
    })
  }
  if (status?.privacy_policy_enabled) {
    items.push({
      key: 'privacy-policy',
      label: t('Privacy Policy'),
      href: '/privacy-policy',
    })
  }
  if (items.length === 0) {
    return null
  }
  return (
    <>
      {items.map((item, index) => (
        <Fragment key={item.key}>
          {(props.leadingSeparator || index > 0) && (
            <span aria-hidden='true' className='text-muted-foreground/30'>
              ·
            </span>
          )}
          <Link
            to={item.href}
            className='hover:text-foreground transition-colors duration-200'
          >
            {item.label}
          </Link>
        </Fragment>
      ))}
    </>
  )
}

// inline=true returns just the inner span for composition in a parent flex
// row. inline=false wraps in a centered/right-aligned div (default).
function ProjectAttribution(props: { currentYear: number; inline?: boolean }) {
  const { t } = useTranslation()
  const content = (
    <span className='text-muted-foreground/45'>
      &copy; {props.currentYear}{' '}
      <a
        href='https://github.com/QuantumNous/new-api'
        target='_blank'
        rel='noopener noreferrer'
        className='text-foreground/70 hover:text-foreground font-medium transition-colors'
      >
        {t('New API')}
      </a>
      . {t(NEW_API_FOOTER_ATTRIBUTION_KEY)}
    </span>
  )
  if (props.inline) {
    return content
  }
  return (
    <div className='text-muted-foreground/45 text-center text-xs sm:text-right'>
      {content}
    </div>
  )
}

export function Footer(props: FooterProps) {
  const { t } = useTranslation()
  const { status } = useStatus()
  const {
    systemName,
    logo: systemLogo,
    footerHtml,
    demoSiteEnabled,
  } = useSystemConfig()

  // Every other admin-authored HTML setting (notice, about) is sanitized before
  // it reaches the DOM; the footer injected the stored markup verbatim, so a
  // script tag saved here executed on every page for every visitor.
  const sanitizedFooterHtml = useMemo(
    () => (footerHtml ? DOMPurify.sanitize(footerHtml) : ''),
    [footerHtml]
  )

  const displayLogo = systemLogo || props.logo || DEFAULT_LOGO
  const displayName = systemName || props.name || DEFAULT_SYSTEM_NAME
  const isDemoSiteMode = Boolean(demoSiteEnabled)
  const currentYear = new Date().getFullYear()
  const version = typeof status?.version === 'string' ? status.version : ''
  const docsLink =
    typeof status?.docs_link === 'string' && status.docs_link
      ? status.docs_link
      : ''

  const fallbackColumns = useMemo<FooterColumnProps[]>(
    () => [
      {
        title: t('footer.columns.about.title'),
        links: [
          {
            text: t('footer.columns.about.links.aboutProject'),
            href: 'https://docs.newapi.pro/wiki/project-introduction/',
          },
          {
            text: t('footer.columns.about.links.contact'),
            href: 'https://docs.newapi.pro/support/community-interaction/',
          },
          {
            text: t('footer.columns.about.links.features'),
            href: 'https://docs.newapi.pro/wiki/features-introduction/',
          },
        ],
      },
      {
        title: t('footer.columns.docs.title'),
        links: [
          {
            text: t('footer.columns.docs.links.quickStart'),
            href: 'https://docs.newapi.pro/getting-started/',
          },
          {
            text: t('footer.columns.docs.links.installation'),
            href: 'https://docs.newapi.pro/installation/',
          },
          {
            text: t('footer.columns.docs.links.apiDocs'),
            href: 'https://docs.newapi.pro/api/',
          },
        ],
      },
      {
        title: t('footer.columns.related.title'),
        links: [
          {
            text: t('footer.columns.related.links.oneApi'),
            href: 'https://github.com/songquanpeng/one-api',
          },
          {
            text: t('footer.columns.related.links.midjourney'),
            href: 'https://github.com/novicezk/midjourney-proxy',
          },
          {
            text: t('footer.columns.related.links.newApiKeyTool'),
            href: 'https://github.com/Calcium-Ion/new-api-key-tool',
          },
        ],
      },
    ],
    [t]
  )

  // The product column points at this deployment's own pages; the demo-site
  // columns (project docs and related repos) are appended only in demo mode,
  // matching the previous gating.
  const productColumn: FooterColumnProps = {
    title: 'Product',
    links: [
      { text: 'Pricing', href: '/pricing' },
      { text: 'Console', href: '/dashboard' },
      ...(docsLink ? [{ text: 'Docs', href: docsLink }] : []),
    ],
  }
  const demoColumns = props.columns ?? fallbackColumns
  const displayColumns = isDemoSiteMode
    ? [productColumn, ...demoColumns]
    : [productColumn]

  if (footerHtml) {
    return (
      <footer
        className={cn(
          'border-border/60 relative z-10 border-t',
          props.className
        )}
      >
        <div className='mx-auto flex w-full max-w-6xl flex-col items-center justify-between gap-4 px-5 py-6 sm:flex-row sm:px-8'>
          <div
            className='custom-footer text-muted-foreground min-w-0 text-center text-sm sm:text-left'
            // eslint-disable-next-line react/no-danger -- sanitized above
            dangerouslySetInnerHTML={{ __html: sanitizedFooterHtml }}
          />
          <div className='text-muted-foreground/45 flex flex-wrap items-center justify-center gap-x-3 gap-y-1 text-xs sm:justify-end'>
            <LegalLinks />
            <ProjectAttribution currentYear={currentYear} inline />
          </div>
        </div>
      </footer>
    )
  }

  return (
    <footer
      className={cn('border-border/60 relative z-10 border-t', props.className)}
    >
      <div className='mx-auto max-w-6xl px-5 py-14 sm:px-8 md:py-16'>
        <div className='grid gap-12 md:grid-cols-[minmax(0,1.4fr)_minmax(0,2.6fr)] md:gap-16'>
          {/* Brand column */}
          <div>
            <Link to='/' className='inline-flex items-center gap-2.5'>
              <img
                src={displayLogo}
                alt={displayName}
                className='size-7 rounded-md object-contain'
              />
              <span className='font-heading text-base font-semibold tracking-tight'>
                {displayName}
              </span>
            </Link>
            <p className='text-muted-foreground mt-4 max-w-[260px] text-sm leading-relaxed'>
              {t('Powerful API Management Platform')}
            </p>
          </div>

          {/* Link columns */}
          <div className='grid grid-cols-2 gap-8 sm:grid-cols-3 lg:auto-cols-fr lg:grid-flow-col'>
            {displayColumns.map((column) => (
              <div key={column.title}>
                <p className='text-primary/80 mb-4 font-mono text-[10px] font-semibold tracking-[0.18em] uppercase'>
                  {t(column.title)}
                </p>
                <ul className='space-y-2.5'>
                  {column.links.map((link) => (
                    <li key={link.href}>
                      <FooterLinkItem link={link} />
                    </li>
                  ))}
                </ul>
              </div>
            ))}
          </div>
        </div>

        {/* Ruled bottom row: copyright + legal + project attribution on the
            left, build version on the right. Wraps on narrow screens. */}
        <div className='border-border/40 mt-14 flex flex-col items-center gap-x-6 gap-y-3 border-t pt-6 text-xs sm:flex-row sm:items-start sm:justify-between'>
          <div className='text-muted-foreground/40 flex flex-wrap items-center justify-center gap-x-2 gap-y-1 sm:justify-start'>
            <span>
              &copy; {currentYear} {displayName}.{' '}
              {props.copyright ?? t('footer.defaultCopyright')}
            </span>
            <LegalLinks leadingSeparator />
            <span aria-hidden='true' className='text-muted-foreground/30'>
              ·
            </span>
            <ProjectAttribution currentYear={currentYear} inline />
          </div>
          {version && (
            <span className='text-muted-foreground/50 shrink-0 font-mono text-[11px] tracking-wider'>
              {version}
            </span>
          )}
        </div>
      </div>
    </footer>
  )
}
