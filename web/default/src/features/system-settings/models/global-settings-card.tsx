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
import { zodResolver } from '@hookform/resolvers/zod'
import { useEffect, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import * as z from 'zod'

import { StatusBadge } from '@/components/status-badge'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Separator } from '@/components/ui/separator'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'
import { api } from '@/lib/api'

import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'

const thinkingBlacklistExample = JSON.stringify(
  ['moonshotai/kimi-k2-thinking', 'kimi-k2-thinking'],
  null,
  2
)

const chatToResponsesPolicyExample = JSON.stringify(
  {
    enabled: true,
    all_channels: false,
    channel_ids: [1, 2],
    model_patterns: ['^gpt-4o.*$', '^gpt-5.*$'],
  },
  null,
  2
)

const chatToResponsesPolicyAllChannelsExample = JSON.stringify(
  {
    enabled: true,
    all_channels: true,
    model_patterns: ['^gpt-4o.*$', '^gpt-5.*$'],
  },
  null,
  2
)

const channelTestPromptsExample = JSON.stringify(
  [
    '用 HTML 和 JavaScript 写一个贪吃蛇小游戏，要求支持键盘控制和计分。',
    '液态玻璃（Liquid Glass）效果在 CSS 里怎么实现？给出关键属性和一个最小示例。',
    'Implement a least-recently-used (LRU) cache in TypeScript with O(1) get and put.',
    '解释一下数据库索引为什么能加快查询，什么情况下反而会变慢。',
  ],
  null,
  2
)

const jsonString = z.string().refine((value) => {
  const trimmed = value.trim()
  if (!trimmed) return true
  try {
    JSON.parse(trimmed)
    return true
  } catch {
    return false
  }
}, 'Invalid JSON format')

const schema = z.object({
  global: z.object({
    pass_through_request_enabled: z.boolean(),
    thinking_model_blacklist: jsonString,
    chat_completions_to_responses_policy: jsonString,
  }),
  general_setting: z.object({
    ping_interval_enabled: z.boolean(),
    ping_interval_seconds: z.coerce.number().min(1),
  }),
  monitor_setting: z.object({
    upstream_model_update_enabled: z.boolean(),
    upstream_model_update_interval_hours: z.coerce.number().min(1),
    upstream_model_update_scan_all_channels: z.boolean(),
    upstream_model_update_validate: z.boolean(),
    upstream_model_update_remove_failed: z.boolean(),
    upstream_model_update_remove_unavailable_models: z.boolean(),
    upstream_model_update_retry_delay_minutes: z.coerce.number().min(1),
    upstream_model_update_failure_threshold: z.coerce.number().min(1),
    upstream_model_update_rotation_sample_size: z.coerce.number().min(0),
    upstream_model_update_max_validations_per_run: z.coerce.number().min(1),
    channel_test_prompts: jsonString,
    channel_test_client_headers: jsonString,
  }),
})

type GlobalModelSettingsFormValues = z.output<typeof schema>
type GlobalModelSettingsFormInput = z.input<typeof schema>

type FlatGlobalModelSettings = {
  'global.pass_through_request_enabled': boolean
  'global.thinking_model_blacklist': string
  'global.chat_completions_to_responses_policy': string
  'general_setting.ping_interval_enabled': boolean
  'general_setting.ping_interval_seconds': number
  'monitor_setting.upstream_model_update_enabled': boolean
  'monitor_setting.upstream_model_update_interval_hours': number
  'monitor_setting.upstream_model_update_scan_all_channels': boolean
  'monitor_setting.upstream_model_update_validate': boolean
  'monitor_setting.upstream_model_update_remove_failed': boolean
  'monitor_setting.upstream_model_update_remove_unavailable_models': boolean
  'monitor_setting.upstream_model_update_retry_delay_minutes': number
  'monitor_setting.upstream_model_update_failure_threshold': number
  'monitor_setting.upstream_model_update_rotation_sample_size': number
  'monitor_setting.upstream_model_update_max_validations_per_run': number
  'monitor_setting.channel_test_prompts': string
  'monitor_setting.channel_test_client_headers': string
}

const flattenGlobalValues = (
  values: GlobalModelSettingsFormValues
): FlatGlobalModelSettings => ({
  'global.pass_through_request_enabled':
    values.global.pass_through_request_enabled,
  'global.thinking_model_blacklist': normalizeJsonText(
    values.global.thinking_model_blacklist,
    '[]'
  ),
  'global.chat_completions_to_responses_policy': normalizeJsonText(
    values.global.chat_completions_to_responses_policy,
    '{}'
  ),
  'general_setting.ping_interval_enabled':
    values.general_setting.ping_interval_enabled,
  'general_setting.ping_interval_seconds':
    values.general_setting.ping_interval_seconds,
  'monitor_setting.upstream_model_update_enabled':
    values.monitor_setting.upstream_model_update_enabled,
  'monitor_setting.upstream_model_update_interval_hours':
    values.monitor_setting.upstream_model_update_interval_hours,
  'monitor_setting.upstream_model_update_scan_all_channels':
    values.monitor_setting.upstream_model_update_scan_all_channels,
  'monitor_setting.upstream_model_update_validate':
    values.monitor_setting.upstream_model_update_validate,
  'monitor_setting.upstream_model_update_remove_failed':
    values.monitor_setting.upstream_model_update_remove_failed,
  'monitor_setting.upstream_model_update_remove_unavailable_models':
    values.monitor_setting.upstream_model_update_remove_unavailable_models,
  'monitor_setting.upstream_model_update_retry_delay_minutes':
    values.monitor_setting.upstream_model_update_retry_delay_minutes,
  'monitor_setting.upstream_model_update_failure_threshold':
    values.monitor_setting.upstream_model_update_failure_threshold,
  'monitor_setting.upstream_model_update_rotation_sample_size':
    values.monitor_setting.upstream_model_update_rotation_sample_size,
  'monitor_setting.upstream_model_update_max_validations_per_run':
    values.monitor_setting.upstream_model_update_max_validations_per_run,
  'monitor_setting.channel_test_prompts': normalizeJsonText(
    values.monitor_setting.channel_test_prompts,
    '[]'
  ),
  'monitor_setting.channel_test_client_headers': normalizeJsonText(
    values.monitor_setting.channel_test_client_headers,
    '{}'
  ),
})

function normalizeJsonText(value: string, fallback: string) {
  const trimmed = (value ?? '').toString().trim()
  return trimmed ? trimmed : fallback
}

// Mirrors controller.clientHeaderPreset on the backend.
type ClientHeaderPreset = {
  id: string
  label: string
  family: string
  endpoint: string
  headers: Record<string, string>
}

const channelTestClientHeadersExample = JSON.stringify(
  {
    claude: { 'user-agent': 'claude-cli/2.1.220 (external, cli)' },
    openai: { 'user-agent': 'OpenAI/Python 2.52.0' },
  },
  null,
  2
)

type GlobalSettingsCardProps = {
  defaultValues: GlobalModelSettingsFormValues
}

export function GlobalSettingsCard({ defaultValues }: GlobalSettingsCardProps) {
  const { t } = useTranslation()
  const updateOption = useUpdateOption()
  const [runningUpdate, setRunningUpdate] = useState(false)
  const [headerPresets, setHeaderPresets] = useState<ClientHeaderPreset[]>([])
  const [selectedPreset, setSelectedPreset] = useState('')

  // The preset list comes from the backend rather than being duplicated here:
  // two hardcoded copies drift, and a preset that does not match what the
  // gateway actually sends is worse than no preset at all.
  useEffect(() => {
    let cancelled = false
    api
      .get('/api/option/channel_test_client_header_presets')
      .then((res) => {
        if (cancelled) return
        if (res.data?.success && Array.isArray(res.data.data)) {
          setHeaderPresets(res.data.data as ClientHeaderPreset[])
        }
      })
      .catch(() => {
        // A missing preset list must not break the settings page: the textarea
        // below still works, the dropdown is just empty.
      })
    return () => {
      cancelled = true
    }
  }, [])

  const presetSelectItems = headerPresets.map((preset) => ({
    value: preset.id,
    label: preset.label,
  }))

  // Merging rather than replacing: an admin who set up openai headers should
  // not lose them by picking a claude preset.
  const applyHeaderPreset = (presetId: string | null) => {
    setSelectedPreset(presetId ?? '')
    const preset = headerPresets.find((item) => item.id === presetId)
    if (!preset) return

    const raw = form.getValues('monitor_setting.channel_test_client_headers')
    let current: Record<string, Record<string, string>> = {}
    if (raw && raw.trim()) {
      try {
        const parsed = JSON.parse(raw)
        if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
          current = parsed as Record<string, Record<string, string>>
        }
      } catch {
        toast.error(t('Invalid JSON format'))
        return
      }
    }

    current[preset.family] = { ...preset.headers }
    form.setValue(
      'monitor_setting.channel_test_client_headers',
      JSON.stringify(current, null, 2),
      { shouldDirty: true }
    )
    toast.success(t('Preset applied'))
  }

  const form = useForm<
    GlobalModelSettingsFormInput,
    unknown,
    GlobalModelSettingsFormValues
  >({
    resolver: zodResolver(schema),
    defaultValues: defaultValues as GlobalModelSettingsFormInput,
  })

  useEffect(() => {
    form.reset(defaultValues as GlobalModelSettingsFormInput)
  }, [defaultValues, form])

  const pingEnabled = form.watch('general_setting.ping_interval_enabled')
  const autoUpdateEnabled = form.watch(
    'monitor_setting.upstream_model_update_enabled'
  )
  // The upstream-rejection rule only widens what counts as a model failure; with
  // removal itself off it would have nothing to act on, so the switch follows it.
  const removeFailedEnabled = form.watch(
    'monitor_setting.upstream_model_update_remove_failed'
  )

  // Trigger the scheduled auto-update immediately. auto_apply distinguishes this
  // from the channels page "detect all" button, which only stages changes.
  const runUpstreamUpdateNow = async () => {
    if (runningUpdate) return
    setRunningUpdate(true)
    try {
      const { data } = await api.post(
        '/api/channel/upstream_updates/detect_all',
        { auto_apply: true },
        { skipBusinessError: true, skipErrorHandler: true }
      )
      if (data?.success) {
        toast.success(t('Model auto-update task started'))
      } else {
        toast.error(data?.message || t('Failed to start model auto-update'))
      }
    } catch (error) {
      const status = (error as { response?: { status?: number } })?.response
        ?.status
      if (status === 409) {
        toast.info(t('A model update task is already running'))
      } else {
        toast.error(t('Failed to start model auto-update'))
      }
    } finally {
      setRunningUpdate(false)
    }
  }

  const formatJsonField = (
    field:
      | 'global.thinking_model_blacklist'
      | 'global.chat_completions_to_responses_policy'
      | 'monitor_setting.channel_test_prompts'
      | 'monitor_setting.channel_test_client_headers'
  ) => {
    const raw = form.getValues(field)
    if (!raw || !raw.trim()) return
    try {
      const formatted = JSON.stringify(JSON.parse(raw), null, 2)
      form.setValue(field, formatted, { shouldDirty: true })
    } catch {
      toast.error(t('Invalid JSON format'))
    }
  }

  const onSubmit = async (values: GlobalModelSettingsFormValues) => {
    const flattenedDefaults = flattenGlobalValues(defaultValues)
    const flattenedValues = flattenGlobalValues(values)
    const updates = Object.entries(flattenedValues).filter(
      ([key, value]) =>
        value !== flattenedDefaults[key as keyof FlatGlobalModelSettings]
    )

    if (updates.length === 0) {
      toast.info(t('No changes to save'))
      return
    }

    for (const [key, value] of updates) {
      await updateOption.mutateAsync({
        key,
        value,
      })
    }
  }

  return (
    <SettingsSection title={t('Global Model Configuration')}>
      <Form {...form}>
        <SettingsForm onSubmit={form.handleSubmit(onSubmit)}>
          <SettingsPageFormActions
            onSave={form.handleSubmit(onSubmit)}
            isSaving={updateOption.isPending}
          />
          <FormField
            control={form.control}
            name='global.pass_through_request_enabled'
            render={({ field }) => (
              <SettingsSwitchItem>
                <SettingsSwitchContent>
                  <FormLabel>{t('Enable Request Passthrough')}</FormLabel>
                  <FormDescription>
                    {t(
                      'Forward requests directly to upstream providers without any post-processing.'
                    )}
                  </FormDescription>
                </SettingsSwitchContent>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                  />
                </FormControl>
              </SettingsSwitchItem>
            )}
          />

          <FormField
            control={form.control}
            name='global.thinking_model_blacklist'
            render={({ field }) => (
              <FormItem>
                <FormLabel>
                  {t('Models that skip thinking suffix processing')}
                </FormLabel>
                <FormControl>
                  <Textarea
                    rows={4}
                    placeholder={`${t('Example:')}\n${thinkingBlacklistExample}`}
                    {...field}
                    onChange={(event) => field.onChange(event.target.value)}
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Models listed here will not automatically append or remove -thinking / -nothinking suffixes.'
                  )}
                </FormDescription>
                <div className='flex flex-wrap gap-2'>
                  <Button
                    type='button'
                    variant='outline'
                    size='sm'
                    onClick={() =>
                      formatJsonField('global.thinking_model_blacklist')
                    }
                  >
                    {t('Format JSON')}
                  </Button>
                </div>
                <FormMessage />
              </FormItem>
            )}
          />

          <Separator />

          <div className='space-y-4'>
            <div className='flex items-center gap-2'>
              <h3 className='text-base font-semibold'>
                {t('ChatCompletions -> Responses Compatibility')}
              </h3>
              <StatusBadge
                label={t('Preview')}
                variant='neutral'
                copyable={false}
              />
            </div>

            <Alert>
              <AlertTitle>{t('Warning')}</AlertTitle>
              <AlertDescription>
                {t(
                  'This feature is experimental. Configuration format and behavior may change.'
                )}
              </AlertDescription>
            </Alert>

            <FormField
              control={form.control}
              name='global.chat_completions_to_responses_policy'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Policy JSON')}</FormLabel>
                  <FormControl>
                    <Textarea
                      rows={8}
                      placeholder={`${t('Example (specific channels):')}\n${chatToResponsesPolicyExample}\n\n${t('Example (all channels):')}\n${chatToResponsesPolicyAllChannelsExample}`}
                      {...field}
                      onChange={(event) => field.onChange(event.target.value)}
                    />
                  </FormControl>
                  <FormDescription>
                    {t('Empty value will be saved as {}.')}
                  </FormDescription>
                  <div className='flex flex-wrap gap-2'>
                    <Button
                      type='button'
                      variant='outline'
                      size='sm'
                      onClick={() =>
                        form.setValue(
                          'global.chat_completions_to_responses_policy',
                          chatToResponsesPolicyExample,
                          { shouldDirty: true }
                        )
                      }
                    >
                      {t('Fill example (specific channels)')}
                    </Button>
                    <Button
                      type='button'
                      variant='outline'
                      size='sm'
                      onClick={() =>
                        form.setValue(
                          'global.chat_completions_to_responses_policy',
                          chatToResponsesPolicyAllChannelsExample,
                          { shouldDirty: true }
                        )
                      }
                    >
                      {t('Fill example (all channels)')}
                    </Button>
                    <Button
                      type='button'
                      variant='outline'
                      size='sm'
                      onClick={() =>
                        formatJsonField(
                          'global.chat_completions_to_responses_policy'
                        )
                      }
                    >
                      {t('Format JSON')}
                    </Button>
                  </div>
                  <FormMessage />
                </FormItem>
              )}
            />
          </div>

          <Separator />

          <FormField
            control={form.control}
            name='general_setting.ping_interval_enabled'
            render={({ field }) => (
              <SettingsSwitchItem>
                <SettingsSwitchContent>
                  <FormLabel>{t('Keep-alive Ping')}</FormLabel>
                  <FormDescription>
                    {t(
                      'Periodically send ping frames to keep streaming connections active.'
                    )}
                  </FormDescription>
                </SettingsSwitchContent>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                  />
                </FormControl>
              </SettingsSwitchItem>
            )}
          />

          <FormField
            control={form.control}
            name='general_setting.ping_interval_seconds'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Ping Interval (seconds)')}</FormLabel>
                <FormControl>
                  <Input
                    type='number'
                    min={1}
                    disabled={!pingEnabled}
                    className='w-24'
                    value={
                      field.value === undefined || field.value === null
                        ? ''
                        : String(field.value)
                    }
                    onChange={(event) => field.onChange(event.target.value)}
                    onBlur={field.onBlur}
                    name={field.name}
                    ref={field.ref}
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Recommended to keep this high to avoid upstream throttling.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <Separator />

          <div className='space-y-4'>
            <h3 className='text-base font-semibold'>
              {t('Automatic Channel Model Update')}
            </h3>

            <FormField
              control={form.control}
              name='monitor_setting.upstream_model_update_enabled'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>{t('Enable Automatic Model Update')}</FormLabel>
                    <FormDescription>
                      {t(
                        'Periodically fetch the upstream model list of every enabled channel, confirm each model with a real request, then add new models and remove the ones that stay broken.'
                      )}
                    </FormDescription>
                  </SettingsSwitchContent>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      onCheckedChange={field.onChange}
                    />
                  </FormControl>
                </SettingsSwitchItem>
              )}
            />

            <FormField
              control={form.control}
              name='monitor_setting.upstream_model_update_interval_hours'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Update Interval (hours)')}</FormLabel>
                  <FormControl>
                    <Input
                      type='number'
                      min={1}
                      disabled={!autoUpdateEnabled}
                      className='w-24'
                      value={
                        field.value === undefined || field.value === null
                          ? ''
                          : String(field.value)
                      }
                      onChange={(event) => field.onChange(event.target.value)}
                      onBlur={field.onBlur}
                      name={field.name}
                      ref={field.ref}
                    />
                  </FormControl>
                  <FormDescription>
                    {t('Defaults to once per day.')}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='monitor_setting.upstream_model_update_scan_all_channels'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>{t('Scan All Enabled Channels')}</FormLabel>
                    <FormDescription>
                      {t(
                        'Ignore the per-channel detection switch and check every channel that is not disabled.'
                      )}
                    </FormDescription>
                  </SettingsSwitchContent>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      disabled={!autoUpdateEnabled}
                      onCheckedChange={field.onChange}
                    />
                  </FormControl>
                </SettingsSwitchItem>
              )}
            />

            <FormField
              control={form.control}
              name='monitor_setting.upstream_model_update_validate'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>{t('Validate Models Before Adding')}</FormLabel>
                    <FormDescription>
                      {t(
                        'Send one real request per model using the channel type matching format. Turning this off adopts the upstream list on trust.'
                      )}
                    </FormDescription>
                  </SettingsSwitchContent>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      disabled={!autoUpdateEnabled}
                      onCheckedChange={field.onChange}
                    />
                  </FormControl>
                </SettingsSwitchItem>
              )}
            />

            <FormField
              control={form.control}
              name='monitor_setting.upstream_model_update_remove_failed'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>{t('Remove Failing Models')}</FormLabel>
                    <FormDescription>
                      {t(
                        'Rate limited models are never removed. Only models that keep failing with a channel fault are dropped, and a channel is never left with no models.'
                      )}
                    </FormDescription>
                  </SettingsSwitchContent>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      disabled={!autoUpdateEnabled}
                      onCheckedChange={field.onChange}
                    />
                  </FormControl>
                </SettingsSwitchItem>
              )}
            />

            <FormField
              control={form.control}
              name='monitor_setting.upstream_model_update_remove_unavailable_models'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>{t('Remove Models the Upstream Rejects')}</FormLabel>
                    <FormDescription>
                      {t(
                        'Also count an explicit HTTP 404 "model not supported" as a model failure. Quota, balance, auth and rate limit errors never count, and neither do 400 or 503 responses, because those are account-level or transient. Off by default: turning it on lets models start being removed on a deployment where this never happened before.'
                      )}
                    </FormDescription>
                  </SettingsSwitchContent>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      disabled={!autoUpdateEnabled || !removeFailedEnabled}
                      onCheckedChange={field.onChange}
                    />
                  </FormControl>
                </SettingsSwitchItem>
              )}
            />

            <FormField
              control={form.control}
              name='monitor_setting.upstream_model_update_retry_delay_minutes'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Retry Delay After Failure (minutes)')}</FormLabel>
                  <FormControl>
                    <Input
                      type='number'
                      min={1}
                      disabled={!autoUpdateEnabled}
                      className='w-24'
                      value={
                        field.value === undefined || field.value === null
                          ? ''
                          : String(field.value)
                      }
                      onChange={(event) => field.onChange(event.target.value)}
                      onBlur={field.onBlur}
                      name={field.name}
                      ref={field.ref}
                    />
                  </FormControl>
                  <FormDescription>
                    {t(
                      'A model that just failed is not re-tested until this much time has passed.'
                    )}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='monitor_setting.upstream_model_update_failure_threshold'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Failures Before Removal')}</FormLabel>
                  <FormControl>
                    <Input
                      type='number'
                      min={1}
                      disabled={!autoUpdateEnabled}
                      className='w-24'
                      value={
                        field.value === undefined || field.value === null
                          ? ''
                          : String(field.value)
                      }
                      onChange={(event) => field.onChange(event.target.value)}
                      onBlur={field.onBlur}
                      name={field.name}
                      ref={field.ref}
                    />
                  </FormControl>
                  <FormDescription>
                    {t(
                      'Consecutive channel-fault failures required before a model is removed.'
                    )}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='monitor_setting.upstream_model_update_rotation_sample_size'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Rotating Sample Size Per Channel')}</FormLabel>
                  <FormControl>
                    <Input
                      type='number'
                      min={0}
                      disabled={!autoUpdateEnabled}
                      className='w-24'
                      value={
                        field.value === undefined || field.value === null
                          ? ''
                          : String(field.value)
                      }
                      onChange={(event) => field.onChange(event.target.value)}
                      onBlur={field.onBlur}
                      name={field.name}
                      ref={field.ref}
                    />
                  </FormControl>
                  <FormDescription>
                    {t(
                      'How many already-serving models each run spot-checks per channel. The cursor advances every run so all models are eventually covered. Set to 0 to disable spot checks.'
                    )}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='monitor_setting.upstream_model_update_max_validations_per_run'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Max Validation Requests Per Run')}</FormLabel>
                  <FormControl>
                    <Input
                      type='number'
                      min={1}
                      disabled={!autoUpdateEnabled}
                      className='w-24'
                      value={
                        field.value === undefined || field.value === null
                          ? ''
                          : String(field.value)
                      }
                      onChange={(event) => field.onChange(event.target.value)}
                      onBlur={field.onBlur}
                      name={field.name}
                      ref={field.ref}
                    />
                  </FormControl>
                  <FormDescription>
                    {t(
                      'Each validation request consumes quota from the root user and writes one usage log entry, so this budget is shared across all channels in a run.'
                    )}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='monitor_setting.channel_test_prompts'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Channel Test Prompts')}</FormLabel>
                  <FormControl>
                    <Textarea
                      rows={6}
                      placeholder={`${t('Example:')}\n${channelTestPromptsExample}`}
                      {...field}
                      onChange={(event) => field.onChange(event.target.value)}
                    />
                  </FormControl>
                  <FormDescription>
                    {t(
                      'A prompt is picked at random for each test request. Leave empty to use the built-in pool. Realistic prompts avoid being flagged as bot probing the way a fixed "hi" is.'
                    )}
                  </FormDescription>
                  <div className='flex flex-wrap gap-2'>
                    <Button
                      type='button'
                      variant='outline'
                      size='sm'
                      onClick={() =>
                        form.setValue(
                          'monitor_setting.channel_test_prompts',
                          channelTestPromptsExample,
                          { shouldDirty: true }
                        )
                      }
                    >
                      {t('Fill built-in examples')}
                    </Button>
                    <Button
                      type='button'
                      variant='outline'
                      size='sm'
                      onClick={() =>
                        formatJsonField('monitor_setting.channel_test_prompts')
                      }
                    >
                      {t('Format JSON')}
                    </Button>
                  </div>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='monitor_setting.channel_test_client_headers'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Channel Test Client Headers')}</FormLabel>
                  <div className='mb-2 flex flex-wrap items-center gap-2'>
                    <Select
                      items={presetSelectItems}
                      value={selectedPreset}
                      onValueChange={applyHeaderPreset}
                    >
                      <SelectTrigger className='w-[420px] max-w-full min-w-0'>
                        <SelectValue
                          className='min-w-0 truncate'
                          placeholder={t('Choose a client preset…')}
                        />
                      </SelectTrigger>
                      <SelectContent
                        alignItemWithTrigger={false}
                        className='w-[420px] max-w-[calc(100vw-2rem)]'
                      >
                        <SelectGroup>
                          {presetSelectItems.map((option) => (
                            <SelectItem
                              key={option.value}
                              value={option.value}
                              className='items-start py-2'
                            >
                              {option.label}
                            </SelectItem>
                          ))}
                        </SelectGroup>
                      </SelectContent>
                    </Select>
                  </div>
                  <FormControl>
                    <Textarea
                      rows={10}
                      placeholder={`${t('Example:')}\n${channelTestClientHeadersExample}`}
                      {...field}
                      onChange={(event) => field.onChange(event.target.value)}
                    />
                  </FormControl>
                  <FormDescription>
                    {t(
                      'Headers sent with channel test requests, so upstreams that only accept a real client (Claude Code, an official SDK) do not reject the test. Grouped by client family: claude / openai / codex / gemini / generic, plus "*" for all. An empty value removes a built-in header. Built-in versions go stale — pick a preset above, then edit.'
                    )}
                  </FormDescription>
                  <div className='flex flex-wrap gap-2'>
                    <Button
                      type='button'
                      variant='outline'
                      size='sm'
                      onClick={() =>
                        formatJsonField(
                          'monitor_setting.channel_test_client_headers'
                        )
                      }
                    >
                      {t('Format JSON')}
                    </Button>
                    <Button
                      type='button'
                      variant='outline'
                      size='sm'
                      onClick={() => {
                        form.setValue(
                          'monitor_setting.channel_test_client_headers',
                          '{}',
                          { shouldDirty: true }
                        )
                        setSelectedPreset('')
                      }}
                    >
                      {t('Reset to built-in')}
                    </Button>
                  </div>
                  <FormMessage />
                </FormItem>
              )}
            />

            <div className='flex flex-wrap gap-2'>
              <Button
                type='button'
                variant='outline'
                size='sm'
                disabled={runningUpdate}
                onClick={runUpstreamUpdateNow}
              >
                {t('Run model update now')}
              </Button>
            </div>
            <FormDescription>
              {t(
                'Runs one update cycle immediately using the saved settings. Save your changes first.'
              )}
            </FormDescription>
          </div>
        </SettingsForm>
      </Form>
    </SettingsSection>
  )
}
