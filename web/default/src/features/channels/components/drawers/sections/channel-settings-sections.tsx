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
import { useFormContext, useWatch } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'

import type { ChannelFormValues } from '../../../lib/channel-form'
import { getChannelSettingsOverview } from '../../../lib/channel-setting-visibility'
import { ChannelClientIdentitySection } from './channel-client-identity-section'
import { ChannelConnectionSection } from './channel-connection-section'
import { ChannelModelDetectionSection } from './channel-model-detection-section'
import { ChannelRequestRewritingSection } from './channel-request-rewriting-section'
import { ChannelSessionCompatSection } from './channel-session-compat-section'

export type ChannelSettingsSectionsProps = {
  sensitiveLocked: boolean
  isEditing: boolean
  /** The channel holds (or is being created with) several keys. */
  multiKey: boolean
  /** Keys in the channel, for the key environment summary. */
  keyCount?: number
  /** The backend found that the upstream refuses the WebSocket upgrade. */
  websocketDowngraded: boolean
  /** What the backend recorded on its last check of the upstream's models. */
  upstreamModelInfo: { lastCheckTime: unknown; detectedModels: string[] }
  upstreamKeepaliveLastTime: unknown
  /** DOM ids of the cards, for the editor's navigator. */
  sectionIds: {
    connection: string
    clientIdentity: string
    requestRewriting: string
    sessionCompat: string
    upstreamModelDetection: string
  }
}

/**
 * The channel-level settings of the channel editor, grouped by what they are
 * for. Each option is shown only on the channel types that use it; one that is
 * set on a type that does not use it stays visible, marked, so it can be
 * turned off. Hiding an option never changes its value.
 */
export function ChannelSettingsSections(props: ChannelSettingsSectionsProps) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const channelType = useWatch({ control: form.control, name: 'type' })
  const values = useWatch({ control: form.control })
  const overview = getChannelSettingsOverview(
    channelType,
    values,
    // The values the channel was opened with. A channel that is being created
    // has nothing stored yet.
    props.isEditing ? form.formState.defaultValues : undefined
  )
  const cards = overview.cards

  return (
    <>
      {props.sensitiveLocked && (
        <Alert className='border-amber-200 bg-amber-50 text-amber-900 dark:border-amber-500/40 dark:bg-amber-500/10 dark:text-amber-50'>
          <AlertDescription>
            {t('No permission to perform this action')}
          </AlertDescription>
        </Alert>
      )}

      {cards.connection.visible && (
        <ChannelConnectionSection
          id={props.sectionIds.connection}
          configured={cards.connection.configured}
          sensitiveLocked={props.sensitiveLocked}
          settings={overview.settings}
          websocketDowngraded={props.websocketDowngraded}
          upstreamKeepaliveLastTime={props.upstreamKeepaliveLastTime}
        />
      )}

      <ChannelClientIdentitySection
        id={props.sectionIds.clientIdentity}
        configured={cards.clientIdentity.configured}
        sensitiveLocked={props.sensitiveLocked}
        multiKey={props.multiKey}
        keyCount={props.keyCount}
      />

      {cards.requestRewriting.visible && (
        <ChannelRequestRewritingSection
          id={props.sectionIds.requestRewriting}
          configured={cards.requestRewriting.configured}
          sensitiveLocked={props.sensitiveLocked}
          settings={overview.settings}
        />
      )}

      {cards.sessionCompat.visible && (
        <ChannelSessionCompatSection
          id={props.sectionIds.sessionCompat}
          configured={cards.sessionCompat.configured}
          sensitiveLocked={props.sensitiveLocked}
          settings={overview.settings}
        />
      )}

      {cards.upstreamModelDetection.visible && (
        <ChannelModelDetectionSection
          id={props.sectionIds.upstreamModelDetection}
          configured={cards.upstreamModelDetection.configured}
          sensitiveLocked={props.sensitiveLocked}
          setting={overview.settings.upstream_model_detection}
          upstreamModelInfo={props.upstreamModelInfo}
        />
      )}
    </>
  )
}
