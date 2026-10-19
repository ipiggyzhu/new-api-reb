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
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { describe, test } from 'node:test'

import {
  CONTENT_LAYOUT_VALUES,
  DEFAULT_THEME_CUSTOMIZATION,
  resolveThemeFont,
  THEME_COOKIE_KEYS,
  THEME_FONT_VALUES,
  THEME_PRESET_VALUES,
  THEME_RADIUS_VALUES,
  THEME_SCALE_VALUES,
  type ThemeCustomization,
} from './theme-customization'

// The inline script in index.html paints the persisted theme before React
// mounts. It cannot import the allowlists, so it restates them; a preset or
// option added in theme-customization.ts but not there would paint the
// default first and then jump once the provider mounts.
const html = readFileSync(new URL('../../index.html', import.meta.url), 'utf8')
const bootScript = html.match(/<script>([\s\S]*?)<\/script>/)?.[1] ?? ''

function runBootScript(cookie: string, systemDark: boolean) {
  const htmlClasses = new Set<string>()
  const bodyAttributes: Record<string, string> = {}
  const document = {
    cookie,
    documentElement: { classList: { add: (c: string) => htmlClasses.add(c) } },
    body: {
      setAttribute: (name: string, value: string) => {
        bodyAttributes[name] = value
      },
    },
  }
  const window = {
    matchMedia: (query: string) => ({
      matches: systemDark && query === '(prefers-color-scheme: dark)',
    }),
  }
  new Function('document', 'window', bootScript)(document, window)
  return { htmlClasses: [...htmlClasses], bodyAttributes }
}

// What ThemeCustomizationProvider leaves on <body> for a customization.
function providerAttributes(customization: ThemeCustomization) {
  const attributes: Record<string, string> = {
    'data-theme-font': resolveThemeFont(
      customization.font,
      customization.preset
    ),
    'data-theme-content-layout': customization.contentLayout,
  }
  if (customization.preset !== 'default') {
    attributes['data-theme-preset'] = customization.preset
  }
  if (customization.radius !== DEFAULT_THEME_CUSTOMIZATION.radius) {
    attributes['data-theme-radius'] = customization.radius
  }
  if (customization.scale !== DEFAULT_THEME_CUSTOMIZATION.scale) {
    attributes['data-theme-scale'] = customization.scale
  }
  return attributes
}

function customizationCookie(customization: ThemeCustomization) {
  return (Object.keys(THEME_COOKIE_KEYS) as (keyof ThemeCustomization)[])
    .map((axis) => `${THEME_COOKIE_KEYS[axis]}=${customization[axis]}`)
    .join('; ')
}

describe('index.html theme boot script', () => {
  test('paints the shipped defaults when nothing is persisted', () => {
    for (const systemDark of [false, true]) {
      const painted = runBootScript('', systemDark)
      assert.deepEqual(painted.htmlClasses, [systemDark ? 'dark' : 'light'])
      assert.deepEqual(
        painted.bodyAttributes,
        providerAttributes(DEFAULT_THEME_CUSTOMIZATION)
      )
    }
  })

  test('paints every persisted option exactly as the provider applies it', () => {
    const customizations: ThemeCustomization[] = []
    for (const preset of THEME_PRESET_VALUES) {
      for (const font of THEME_FONT_VALUES) {
        customizations.push({ ...DEFAULT_THEME_CUSTOMIZATION, preset, font })
      }
    }
    for (const radius of THEME_RADIUS_VALUES) {
      customizations.push({ ...DEFAULT_THEME_CUSTOMIZATION, radius })
    }
    for (const scale of THEME_SCALE_VALUES) {
      customizations.push({ ...DEFAULT_THEME_CUSTOMIZATION, scale })
    }
    for (const contentLayout of CONTENT_LAYOUT_VALUES) {
      customizations.push({ ...DEFAULT_THEME_CUSTOMIZATION, contentLayout })
    }

    for (const customization of customizations) {
      const painted = runBootScript(
        `vite-ui-theme=dark; ${customizationCookie(customization)}`,
        false
      )
      assert.deepEqual(
        painted.bodyAttributes,
        providerAttributes(customization),
        JSON.stringify(customization)
      )
    }
  })

  test('an explicit mode wins over the system and an unknown one follows it', () => {
    for (const [cookie, systemDark, expected] of [
      ['vite-ui-theme=light', true, 'light'],
      ['vite-ui-theme=dark', false, 'dark'],
      ['vite-ui-theme=system', true, 'dark'],
      ['vite-ui-theme=sepia', false, 'light'],
      ['vite-ui-theme=sepia', true, 'dark'],
    ] as const) {
      assert.deepEqual(runBootScript(cookie, systemDark).htmlClasses, [
        expected,
      ])
    }
  })

  test('falls back to the defaults for values outside the allowlists', () => {
    const painted = runBootScript(
      'theme_preset=aurora; theme_font=mono; theme_radius=2xl; theme_scale=xs; theme_content_layout=wide',
      false
    )
    assert.deepEqual(
      painted.bodyAttributes,
      providerAttributes(DEFAULT_THEME_CUSTOMIZATION)
    )
  })
})
