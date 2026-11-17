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
import { ArrowLeftRight } from 'lucide-react'
import { useFormContext } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'

import type { ChannelFormValues } from '../../../lib/channel-form'
import {
  CHANNEL_REWRITING_OPTIONS,
  getSystemPromptScope,
  hasVisibleChannelSetting,
  showRewritingSubHeadings,
  type ChannelSettingViews,
  type SystemPromptScope,
} from '../../../lib/channel-setting-visibility'
import { ChannelForwardedFields } from './channel-forwarded-fields'
import { SubHeading } from './channel-section-heading'
import { ChannelSettingRow } from './channel-setting-row'
import { ChannelSettingsCard } from './channel-settings-card'

type ChannelRequestRewritingSectionProps = {
  id: string
  configured: boolean
  sensitiveLocked: boolean
  channelType: number
  settings: ChannelSettingViews
}

export function ChannelRequestRewritingSection(
  props: ChannelRequestRewritingSectionProps
) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const settings = props.settings
  const subHeadings = showRewritingSubHeadings(settings)
  const systemPromptReason = t(
    'No request path on this channel type applies a channel system prompt.'
  )
  const systemPromptScope = getSystemPromptScope(props.channelType)
  const systemPromptScopeHints: Record<SystemPromptScope, string> = {
    messages: t(
      'On this channel type it is applied to /v1/messages requests only.'
    ),
    responses: t(
      'On this channel type it is applied to /v1/responses requests.'
    ),
    chatAndMessages: t(
      'On this channel type it is applied to /v1/chat/completions and /v1/messages requests; /v1/responses requests are sent without it.'
    ),
  }

  return (
    <ChannelSettingsCard
      id={props.id}
      title={t('Request & response rewriting')}
      description={t(
        'What the gateway changes before forwarding a request and before returning the answer.'
      )}
      icon={<ArrowLeftRight className='h-4 w-4' />}
      iconTone='chart-1'
      configured={props.configured}
      sensitiveLocked={props.sensitiveLocked}
    >
      {hasVisibleChannelSetting(
        settings,
        CHANNEL_REWRITING_OPTIONS.request
      ) && (
        <div className='space-y-4'>
          {subHeadings && <SubHeading title={t('Request')} />}

          {settings.pass_through_body_enabled.state !== 'hidden' && (
            <div className='divide-border space-y-0 divide-y border-y'>
              <ChannelSettingRow setting={settings.pass_through_body_enabled}>
                <FormField
                  control={form.control}
                  name='pass_through_body_enabled'
                  render={({ field }) => (
                    <FormItem className='flex items-center justify-between px-4 py-3'>
                      <div className='space-y-0.5'>
                        <FormLabel>{t('Pass Through Body')}</FormLabel>
                        <FormDescription>
                          {t(
                            "Forward the client's request body as received. While this is on, the system prompt, field filtering, tool-result image handling and parameter overrides are skipped, and model mapping does not rewrite the model name in the body."
                          )}
                        </FormDescription>
                      </div>
                      <FormControl>
                        <Switch
                          checked={field.value}
                          onCheckedChange={field.onChange}
                        />
                      </FormControl>
                    </FormItem>
                  )}
                />
              </ChannelSettingRow>
            </div>
          )}

          <ChannelSettingRow
            setting={settings.system_prompt}
            reason={systemPromptReason}
            layout='block'
          >
            <FormField
              control={form.control}
              name='system_prompt'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('System Prompt')}</FormLabel>
                  <FormControl>
                    <Textarea
                      placeholder={t(
                        'Enter system prompt (user prompt takes priority)'
                      )}
                      rows={3}
                      {...field}
                    />
                  </FormControl>
                  <FormDescription>
                    {t('Used as the system prompt when a request has none.')}
                  </FormDescription>
                  {systemPromptScope && (
                    <p className='text-muted-foreground text-sm'>
                      {systemPromptScopeHints[systemPromptScope]}
                    </p>
                  )}
                  <FormMessage />
                </FormItem>
              )}
            />
          </ChannelSettingRow>

          <ChannelSettingRow
            setting={settings.system_prompt_override}
            reason={systemPromptReason}
            layout='block'
          >
            <FormField
              control={form.control}
              name='system_prompt_override'
              render={({ field }) => (
                <FormItem className='flex items-center justify-between'>
                  <div className='space-y-0.5'>
                    <FormLabel>
                      {t('Also prepend to requests that have their own')}
                    </FormLabel>
                    <FormDescription>
                      {t(
                        'Off: requests that bring their own system prompt are left unchanged.'
                      )}
                    </FormDescription>
                  </div>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      onCheckedChange={field.onChange}
                    />
                  </FormControl>
                </FormItem>
              )}
            />
          </ChannelSettingRow>

          <ChannelForwardedFields settings={settings} />

          {settings.claude_beta_query.state !== 'hidden' && (
            <div className='divide-border space-y-0 divide-y border-y'>
              <ChannelSettingRow
                setting={settings.claude_beta_query}
                reason={t('Only Anthropic channels use this query parameter.')}
              >
                <FormField
                  control={form.control}
                  name='claude_beta_query'
                  render={({ field }) => (
                    <FormItem className='flex items-center justify-between gap-3 px-4 py-3'>
                      <div className='space-y-0.5'>
                        <FormLabel className='text-sm'>
                          {t('Append ?beta=true to Messages requests')}
                        </FormLabel>
                        <FormDescription>
                          {t(
                            'Adds the beta query to the upstream /v1/messages URL. A Claude Code client identity with a pinned version already adds it.'
                          )}
                        </FormDescription>
                      </div>
                      <FormControl>
                        <Switch
                          checked={field.value}
                          onCheckedChange={field.onChange}
                        />
                      </FormControl>
                    </FormItem>
                  )}
                />
              </ChannelSettingRow>
            </div>
          )}
        </div>
      )}

      {hasVisibleChannelSetting(
        settings,
        CHANNEL_REWRITING_OPTIONS.response
      ) && (
        <div className='space-y-4 pt-2'>
          {subHeadings && <SubHeading title={t('Response')} />}

          <div className='divide-border space-y-0 divide-y border-y'>
            <ChannelSettingRow
              setting={settings.force_format}
              reason={t(
                'Only acts on Chat Completions answers from OpenAI-compatible and Gemini upstreams.'
              )}
            >
              <FormField
                control={form.control}
                name='force_format'
                render={({ field }) => (
                  <FormItem className='flex items-center justify-between px-4 py-3'>
                    <div className='space-y-0.5'>
                      <FormLabel>{t('Force Format')}</FormLabel>
                      <FormDescription>
                        {t(
                          'Re-serialise Chat Completions answers into the standard OpenAI format. Turn it on when an upstream returns non-standard fields that break clients.'
                        )}
                      </FormDescription>
                    </div>
                    <FormControl>
                      <Switch
                        checked={field.value}
                        onCheckedChange={field.onChange}
                      />
                    </FormControl>
                  </FormItem>
                )}
              />
            </ChannelSettingRow>

            <ChannelSettingRow
              setting={settings.thinking_to_content}
              reason={t(
                'Only acts on streamed Chat Completions answers from OpenAI-compatible and Gemini upstreams.'
              )}
            >
              <FormField
                control={form.control}
                name='thinking_to_content'
                render={({ field }) => (
                  <FormItem className='flex items-center justify-between px-4 py-3'>
                    <div className='space-y-0.5'>
                      <FormLabel>{t('Thinking to Content')}</FormLabel>
                      <FormDescription>
                        {t(
                          'Convert reasoning_content to <think> tag in content'
                        )}
                      </FormDescription>
                    </div>
                    <FormControl>
                      <Switch
                        checked={field.value}
                        onCheckedChange={field.onChange}
                      />
                    </FormControl>
                  </FormItem>
                )}
              />
            </ChannelSettingRow>
          </div>
        </div>
      )}
    </ChannelSettingsCard>
  )
}
