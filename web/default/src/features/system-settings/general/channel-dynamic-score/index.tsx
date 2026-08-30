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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

import { SettingsSwitchField } from '../../components/settings-form-layout'
import { SettingsSection } from '../../components/settings-section'
import { useUpdateOption } from '../../hooks/use-update-option'
import { DynamicScoreTableDialog } from './score-table-dialog'
import type { ChannelDynamicScoreSettings } from './types'

interface Props {
  defaultValues: ChannelDynamicScoreSettings
}

/** Field keys shared by the form state and the option keys they persist to. */
const NUMERIC_FIELDS = [
  'successes_to_promote',
  'faults_to_demote',
  'max_promote_tiers',
  'max_demote_tiers',
  'min_sample_for_weight',
  'success_window_seconds',
  'idle_reset_seconds',
] as const

type NumericField = (typeof NUMERIC_FIELDS)[number]

type NumericState = Record<NumericField, string>

const FIELD_DEFAULTS: Record<NumericField, number> = {
  successes_to_promote: 5,
  faults_to_demote: 1,
  max_promote_tiers: 1,
  max_demote_tiers: 3,
  min_sample_for_weight: 20,
  success_window_seconds: 300,
  idle_reset_seconds: 1800,
}

function readNumber(
  values: ChannelDynamicScoreSettings,
  field: NumericField
): string {
  const raw = values[`channel_dynamic_score_setting.${field}`]
  if (raw === undefined || raw === null || raw === '') {
    return String(FIELD_DEFAULTS[field])
  }
  return String(raw)
}

export function ChannelDynamicScoreSection(props: Props) {
  const { t } = useTranslation()
  const updateOption = useUpdateOption()

  const [enabled, setEnabled] = useState(
    Boolean(props.defaultValues['channel_dynamic_score_setting.enabled'])
  )
  const [numbers, setNumbers] = useState<NumericState>(() => {
    const initial = {} as NumericState
    for (const field of NUMERIC_FIELDS) {
      initial[field] = readNumber(props.defaultValues, field)
    }
    return initial
  })
  const [saving, setSaving] = useState(false)
  const [scoresOpen, setScoresOpen] = useState(false)

  const setField = (field: NumericField, value: string) => {
    setNumbers((prev) => ({ ...prev, [field]: value }))
  }

  const handleSave = async () => {
    setSaving(true)
    try {
      const updates: { key: string; value: string }[] = []

      if (
        enabled !==
        Boolean(props.defaultValues['channel_dynamic_score_setting.enabled'])
      ) {
        updates.push({
          key: 'channel_dynamic_score_setting.enabled',
          value: String(enabled),
        })
      }

      for (const field of NUMERIC_FIELDS) {
        const parsed = Number.parseInt(numbers[field], 10)
        if (!Number.isFinite(parsed) || parsed < 0) {
          toast.error(t('Please enter a non-negative whole number'))
          setSaving(false)
          return
        }
        if (String(parsed) !== readNumber(props.defaultValues, field)) {
          updates.push({
            key: `channel_dynamic_score_setting.${field}`,
            value: String(parsed),
          })
        }
      }

      if (updates.length === 0) {
        toast.info(t('No changes'))
        return
      }
      for (const update of updates) {
        await updateOption.mutateAsync(update)
      }
      toast.success(t('Saved successfully'))
    } catch {
      toast.error(t('Failed to save'))
    } finally {
      setSaving(false)
    }
  }

  const numberField = (
    field: NumericField,
    label: string,
    description: string
  ) => (
    <div className='space-y-1.5'>
      <Label className='text-sm font-medium' htmlFor={`dynamic-score-${field}`}>
        {label}
      </Label>
      <Input
        id={`dynamic-score-${field}`}
        type='number'
        min={0}
        value={numbers[field]}
        disabled={!enabled}
        onChange={(e) => setField(field, e.target.value)}
      />
      <p className='text-muted-foreground text-xs'>{description}</p>
    </div>
  )

  return (
    <SettingsSection title={t('Dynamic Channel Priority')}>
      <div className='space-y-4'>
        <SettingsSwitchField
          checked={enabled}
          onCheckedChange={setEnabled}
          label={t('Enable dynamic priority and weight')}
          description={t(
            'Channels that keep succeeding rise in the selection order, and channels that fault sink. Your configured priority and weight stay the baseline and are never overwritten; adjustments live in memory only and reset on restart.'
          )}
        />

        <Alert>
          <AlertDescription>
            {t(
              'Only genuine upstream faults count against a channel. Rate limits, client errors and configuration problems on our side are excluded, as is synthetic channel-test traffic.'
            )}
          </AlertDescription>
        </Alert>

        <div className='grid gap-4 sm:grid-cols-2'>
          {numberField(
            'successes_to_promote',
            t('Successes to promote'),
            t(
              'Consecutive successes needed to move a channel up one priority tier. A single fault restarts the streak.'
            )
          )}
          {numberField(
            'faults_to_demote',
            t('Faults to demote'),
            t(
              'Consecutive faults needed to move a channel down one priority tier.'
            )
          )}
          {numberField(
            'max_promote_tiers',
            t('Maximum promotion tiers'),
            t('Upper bound on how far a channel can rise above its baseline.')
          )}
          {numberField(
            'max_demote_tiers',
            t('Maximum demotion tiers'),
            t(
              'Upper bound on how far a channel can sink below its baseline. Movement is counted in tiers, so it works whatever priority numbers you use.'
            )
          )}
          {numberField(
            'min_sample_for_weight',
            t('Minimum requests for weight adjustment'),
            t(
              'Requests a channel needs inside the window before its success rate affects weight. Below this the weight is left exactly as configured.'
            )
          )}
          {numberField(
            'success_window_seconds',
            t('Success rate window (seconds)'),
            t(
              'How far back the success rate looks. Older requests age out instead of counting forever.'
            )
          )}
          {numberField(
            'idle_reset_seconds',
            t('Idle reset (seconds)'),
            t(
              'A channel with no traffic for this long returns to its configured baseline.'
            )
          )}
        </div>

        <div className='flex justify-end gap-2'>
          <Button variant='outline' onClick={() => setScoresOpen(true)}>
            {t('View Current Scores')}
          </Button>
          <Button onClick={handleSave} disabled={saving}>
            {t('Save')}
          </Button>
        </div>
      </div>

      <DynamicScoreTableDialog
        open={scoresOpen}
        onOpenChange={setScoresOpen}
      />
    </SettingsSection>
  )
}
