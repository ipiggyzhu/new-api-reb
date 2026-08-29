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
import { ChannelAffinitySection } from '../general/channel-affinity'
import { ChannelDynamicScoreSection } from '../general/channel-dynamic-score'
import { IoNetDeploymentSettingsSection } from '../integrations/ionet-deployment-settings-section'
import type { ModelSettings } from '../types'
import { createSectionRegistry } from '../utils/section-registry'
import { ClaudeSettingsCard } from './claude-settings-card'
import { GeminiSettingsCard } from './gemini-settings-card'
import { GlobalSettingsCard } from './global-settings-card'
import { GrokSettingsCard } from './grok-settings-card'
import { RoutingReliabilitySection } from './routing-reliability-section'

function formatJsonForEditor(value: string, fallback: string) {
  const raw = (value ?? '').toString().trim()
  if (!raw) return fallback
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return fallback
  }
}

const MODELS_SECTIONS = [
  {
    id: 'global',
    titleKey: 'Global Model Configuration',
    build: (settings: ModelSettings) => (
      <GlobalSettingsCard
        defaultValues={{
          global: {
            pass_through_request_enabled:
              settings['global.pass_through_request_enabled'],
            thinking_model_blacklist: formatJsonForEditor(
              settings['global.thinking_model_blacklist'],
              '[]'
            ),
            chat_completions_to_responses_policy: formatJsonForEditor(
              settings['global.chat_completions_to_responses_policy'],
              '{}'
            ),
          },
          general_setting: {
            ping_interval_enabled:
              settings['general_setting.ping_interval_enabled'],
            ping_interval_seconds:
              settings['general_setting.ping_interval_seconds'],
          },
          monitor_setting: {
            upstream_model_update_enabled:
              settings['monitor_setting.upstream_model_update_enabled'],
            upstream_model_update_interval_hours:
              settings['monitor_setting.upstream_model_update_interval_hours'],
            upstream_model_update_scan_all_channels:
              settings[
                'monitor_setting.upstream_model_update_scan_all_channels'
              ],
            upstream_model_update_validate:
              settings['monitor_setting.upstream_model_update_validate'],
            upstream_model_update_remove_failed:
              settings['monitor_setting.upstream_model_update_remove_failed'],
            upstream_model_update_retry_delay_minutes:
              settings[
                'monitor_setting.upstream_model_update_retry_delay_minutes'
              ],
            upstream_model_update_failure_threshold:
              settings[
                'monitor_setting.upstream_model_update_failure_threshold'
              ],
            upstream_model_update_rotation_sample_size:
              settings[
                'monitor_setting.upstream_model_update_rotation_sample_size'
              ],
            upstream_model_update_max_validations_per_run:
              settings[
                'monitor_setting.upstream_model_update_max_validations_per_run'
              ],
            channel_test_prompts: formatJsonForEditor(
              settings['monitor_setting.channel_test_prompts'],
              '[]'
            ),
            channel_test_client_headers: formatJsonForEditor(
              settings['monitor_setting.channel_test_client_headers'],
              '{}'
            ),
          },
        }}
      />
    ),
  },
  {
    id: 'routing-reliability',
    titleKey: 'Routing Reliability',
    build: (settings: ModelSettings) => (
      <RoutingReliabilitySection
        defaultValues={{
          RetryTimes: settings.RetryTimes,
          ChannelDisableThreshold: settings.ChannelDisableThreshold,
          AutomaticDisableChannelEnabled:
            settings.AutomaticDisableChannelEnabled,
          AutomaticEnableChannelEnabled: settings.AutomaticEnableChannelEnabled,
          AutomaticDisableKeywords: settings.AutomaticDisableKeywords,
          AutomaticDisableStatusCodes: settings.AutomaticDisableStatusCodes,
          AutomaticRetryStatusCodes: settings.AutomaticRetryStatusCodes,
          'monitor_setting.auto_test_channel_enabled':
            settings['monitor_setting.auto_test_channel_enabled'],
          'monitor_setting.auto_test_channel_minutes':
            settings['monitor_setting.auto_test_channel_minutes'],
          'monitor_setting.channel_test_mode':
            settings['monitor_setting.channel_test_mode'],
        }}
      />
    ),
  },
  {
    id: 'gemini',
    titleKey: 'Gemini',
    build: (settings: ModelSettings) => (
      <GeminiSettingsCard
        defaultValues={{
          gemini: {
            safety_settings: settings['gemini.safety_settings'],
            version_settings: settings['gemini.version_settings'],
            supported_imagine_models:
              settings['gemini.supported_imagine_models'],
            thinking_adapter_enabled:
              settings['gemini.thinking_adapter_enabled'],
            thinking_adapter_budget_tokens_percentage:
              settings['gemini.thinking_adapter_budget_tokens_percentage'],
            function_call_thought_signature_enabled:
              settings['gemini.function_call_thought_signature_enabled'],
            remove_function_response_id_enabled:
              settings['gemini.remove_function_response_id_enabled'],
          },
        }}
      />
    ),
  },
  {
    id: 'claude',
    titleKey: 'Claude',
    build: (settings: ModelSettings) => (
      <ClaudeSettingsCard
        defaultValues={{
          claude: {
            model_headers_settings: settings['claude.model_headers_settings'],
            default_max_tokens: settings['claude.default_max_tokens'],
            thinking_adapter_enabled:
              settings['claude.thinking_adapter_enabled'],
            thinking_adapter_budget_tokens_percentage:
              settings['claude.thinking_adapter_budget_tokens_percentage'],
          },
        }}
      />
    ),
  },
  {
    id: 'grok',
    titleKey: 'Grok',
    build: (settings: ModelSettings) => (
      <GrokSettingsCard
        defaultValues={{
          'grok.violation_deduction_enabled':
            settings['grok.violation_deduction_enabled'] ?? true,
          'grok.violation_deduction_amount':
            settings['grok.violation_deduction_amount'] ?? 0.05,
        }}
      />
    ),
  },
  {
    id: 'channel-affinity',
    titleKey: 'Channel Affinity',
    build: (settings: ModelSettings) => (
      <ChannelAffinitySection
        defaultValues={{
          'channel_affinity_setting.enabled':
            settings['channel_affinity_setting.enabled'],
          'channel_affinity_setting.switch_on_success':
            settings['channel_affinity_setting.switch_on_success'],
          'channel_affinity_setting.keep_on_channel_disabled':
            settings['channel_affinity_setting.keep_on_channel_disabled'],
          'channel_affinity_setting.max_entries':
            settings['channel_affinity_setting.max_entries'],
          'channel_affinity_setting.default_ttl_seconds':
            settings['channel_affinity_setting.default_ttl_seconds'],
          'channel_affinity_setting.rules':
            settings['channel_affinity_setting.rules'],
        }}
      />
    ),
  },
  {
    id: 'channel-dynamic-score',
    titleKey: 'Dynamic Channel Priority',
    build: (settings: ModelSettings) => (
      <ChannelDynamicScoreSection
        defaultValues={{
          'channel_dynamic_score_setting.enabled':
            settings['channel_dynamic_score_setting.enabled'],
          'channel_dynamic_score_setting.successes_to_promote':
            settings['channel_dynamic_score_setting.successes_to_promote'],
          'channel_dynamic_score_setting.faults_to_demote':
            settings['channel_dynamic_score_setting.faults_to_demote'],
          'channel_dynamic_score_setting.max_promote_tiers':
            settings['channel_dynamic_score_setting.max_promote_tiers'],
          'channel_dynamic_score_setting.max_demote_tiers':
            settings['channel_dynamic_score_setting.max_demote_tiers'],
          'channel_dynamic_score_setting.min_sample_for_weight':
            settings['channel_dynamic_score_setting.min_sample_for_weight'],
          'channel_dynamic_score_setting.success_window_seconds':
            settings['channel_dynamic_score_setting.success_window_seconds'],
          'channel_dynamic_score_setting.idle_reset_seconds':
            settings['channel_dynamic_score_setting.idle_reset_seconds'],
        }}
      />
    ),
  },
  {
    id: 'model-deployment',
    titleKey: 'Model Deployment',
    build: (settings: ModelSettings) => (
      <IoNetDeploymentSettingsSection
        defaultValues={{
          enabled: settings['model_deployment.ionet.enabled'],
          apiKey: settings['model_deployment.ionet.api_key'],
        }}
      />
    ),
  },
] as const

export type ModelSectionId = (typeof MODELS_SECTIONS)[number]['id']

const modelsRegistry = createSectionRegistry<ModelSectionId, ModelSettings>({
  sections: MODELS_SECTIONS,
  defaultSection: 'global',
  basePath: '/system-settings/models',
  urlStyle: 'path',
})

export const MODELS_SECTION_IDS = modelsRegistry.sectionIds
export const MODELS_DEFAULT_SECTION = modelsRegistry.defaultSection
export const getModelsSectionNavItems = modelsRegistry.getSectionNavItems
export const getModelsSectionContent = modelsRegistry.getSectionContent
export const getModelsSectionMeta = modelsRegistry.getSectionMeta
