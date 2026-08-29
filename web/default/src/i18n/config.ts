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
import i18n, { type BackendModule, type ReadCallback } from 'i18next'
import LanguageDetector from 'i18next-browser-languagedetector'
import { initReactI18next } from 'react-i18next'

import { convertDetectedLanguage } from './languages'

type TranslationBundle = Record<string, unknown>

/**
 * Locale files are ~350-530 KB of raw JSON each. Statically importing all seven
 * put every translation into the entry chunk, so a user downloaded six
 * languages they cannot read before the app rendered. Each entry here becomes
 * its own async chunk that is only fetched for the active language (plus `en`,
 * which stays the fallback because 67 keys are not identity-mapped).
 */
const localeLoaders: Record<
  string,
  () => Promise<{ default: { translation: TranslationBundle } }>
> = {
  en: () => import('./locales/en.json'),
  zhCN: () => import('./locales/zh.json'),
  zhTW: () => import('./locales/zh-TW.json'),
  fr: () => import('./locales/fr.json'),
  ru: () => import('./locales/ru.json'),
  ja: () => import('./locales/ja.json'),
  vi: () => import('./locales/vi.json'),
}

export const SUPPORTED_LANGUAGES = Object.keys(localeLoaders)

// A backend (rather than an ad hoc addResourceBundle call) is what makes
// i18n.changeLanguage() wait for the new locale, so switching languages never
// flashes untranslated keys.
const lazyLocaleBackend: BackendModule = {
  type: 'backend',
  init: () => {},
  read(language: string, _namespace: string, callback: ReadCallback) {
    const load = localeLoaders[language]
    if (!load) {
      callback(null, {})
      return
    }
    void (async () => {
      try {
        const module = await load()
        callback(null, module.default.translation)
      } catch (error) {
        callback(error as Error, false)
      }
    })()
  },
}

export const i18nReady = i18n
  .use(lazyLocaleBackend)
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    fallbackLng: 'en',
    supportedLngs: SUPPORTED_LANGUAGES,
    load: 'currentOnly',
    nsSeparator: false, // Allow literal colons in keys (e.g., URLs, labels)
    debug: import.meta.env.DEV,
    interpolation: {
      escapeValue: false, // not needed for react as it escapes by default
    },
    // Rendering is gated on i18nReady, so Suspense is not needed and would only
    // add a second loading state on top of the one main.tsx already awaits.
    react: { useSuspense: false },
    detection: {
      order: ['localStorage', 'navigator'],
      caches: ['localStorage'],
      // Browsers report `zh-CN`/`zh-TW`/`zh`; map them onto our `zhCN`/`zhTW`
      // codes (non-Chinese codes pass through for normal supportedLngs matching).
      convertDetectedLanguage,
    },
  })

export default i18n
