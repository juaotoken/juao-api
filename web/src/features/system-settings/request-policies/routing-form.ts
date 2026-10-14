import type { TFunction } from 'i18next'
import { z } from 'zod'

import { parseHttpStatusCodeRules } from '@/lib/http-status-code-rules'

// One row of the per-group retry override editor. Empty fields inherit the
// global retry budget / status codes. Stored as the GroupRetryOverrides JSON
// option: { "<group>": { "retry_times"?: number, "status_codes"?: string } }.
export type GroupRetryOverrideRow = {
  group: string
  retry_times: string
  status_codes: string
}

export function createRoutingPolicySchema(t: TFunction) {
  return z.object({
    RetryTimes: z.number().int().min(0).max(99),
    AutomaticRetryStatusCodes: z
      .string()
      .refine(
        (value) => parseHttpStatusCodeRules(value).ok,
        t('Invalid status code rules')
      ),
    GroupRetryOverrides: z
      .array(
        z.object({
          group: z
            .string()
            .trim()
            .min(1, t('Select a group'))
            .refine(
              (value) => value !== 'auto',
              t('The auto group always uses the global retry settings')
            ),
          retry_times: z
            .string()
            .refine(
              (value) =>
                value.trim() === '' ||
                (/^\d+$/.test(value.trim()) && Number(value) <= 99),
              t('Enter an integer from 0 to 99, or leave empty')
            ),
          status_codes: z
            .string()
            .refine(
              (value) => parseHttpStatusCodeRules(value).ok,
              t('Invalid status code rules')
            ),
        })
      )
      .superRefine((rows, context) => {
        const seen = new Set<string>()
        rows.forEach((row, index) => {
          const group = row.group.trim()
          if (group !== '' && seen.has(group)) {
            context.addIssue({
              code: 'custom',
              path: [index, 'group'],
              message: t('This group is already configured'),
            })
          }
          seen.add(group)
        })
      }),
    channel_affinity_setting: z.object({
      enabled: z.boolean(),
      session_mode: z.enum(['', 'off', 'prefer', 'strict']),
      switch_on_success: z.boolean(),
      keep_on_channel_disabled: z.boolean(),
      max_entries: z.number().int().min(0),
      default_ttl_seconds: z.number().int().min(0),
      rules: z.string().superRefine((value, context) => {
        try {
          const rules: unknown = JSON.parse(value)
          if (!Array.isArray(rules)) {
            context.addIssue({
              code: 'custom',
              message: t('Rules JSON must be an array'),
            })
          }
        } catch {
          context.addIssue({
            code: 'custom',
            message: t('Invalid rules JSON format'),
          })
        }
      }),
    }),
  })
}

export type RoutingPolicyFormValues = z.infer<
  ReturnType<typeof createRoutingPolicySchema>
>

export function routingPolicyFormValues(
  options: Record<string, string>
): RoutingPolicyFormValues {
  return {
    RetryTimes: Number(options.RetryTimes),
    AutomaticRetryStatusCodes: options.AutomaticRetryStatusCodes,
    GroupRetryOverrides: groupRetryOverrideRows(options.GroupRetryOverrides),
    channel_affinity_setting: {
      enabled: options['channel_affinity_setting.enabled'] === 'true',
      session_mode: (options['channel_affinity_setting.session_mode'] ||
        '') as RoutingPolicyFormValues['channel_affinity_setting']['session_mode'],
      switch_on_success:
        options['channel_affinity_setting.switch_on_success'] === 'true',
      keep_on_channel_disabled:
        options['channel_affinity_setting.keep_on_channel_disabled'] === 'true',
      max_entries: Number(options['channel_affinity_setting.max_entries']),
      default_ttl_seconds: Number(
        options['channel_affinity_setting.default_ttl_seconds']
      ),
      rules: options['channel_affinity_setting.rules'] || '[]',
    },
  }
}

export function routingPolicyOptions(
  values: RoutingPolicyFormValues
): Record<string, string> {
  return {
    RetryTimes: String(values.RetryTimes),
    AutomaticRetryStatusCodes: values.AutomaticRetryStatusCodes,
    GroupRetryOverrides: groupRetryOverridesOption(values.GroupRetryOverrides),
    ...Object.fromEntries(
      Object.entries(values.channel_affinity_setting).map(([key, value]) => [
        `channel_affinity_setting.${key}`,
        String(value),
      ])
    ),
  }
}

type GroupRetryOverrideValue = { retry_times?: number; status_codes?: string }

function groupRetryOverrideRows(value: string | undefined) {
  let parsed: unknown = {}
  try {
    parsed = JSON.parse(value || '{}')
  } catch {
    parsed = {}
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return []
  return Object.entries(parsed as Record<string, GroupRetryOverrideValue>)
    .map(
      ([group, override]): GroupRetryOverrideRow => ({
        group,
        retry_times:
          typeof override?.retry_times === 'number'
            ? String(override.retry_times)
            : '',
        status_codes: override?.status_codes ?? '',
      })
    )
    .sort((a, b) => a.group.localeCompare(b.group))
}

// Serializes rows with sorted keys and normalized status codes, so an
// unchanged editor produces the same string for the stored option.
function groupRetryOverridesOption(rows: GroupRetryOverrideRow[]) {
  const sorted = [...rows].sort((a, b) =>
    a.group.trim().localeCompare(b.group.trim())
  )
  const result: Record<string, GroupRetryOverrideValue> = {}
  for (const row of sorted) {
    const override: GroupRetryOverrideValue = {}
    if (row.retry_times.trim() !== '') {
      override.retry_times = Number(row.retry_times)
    }
    const codes = parseHttpStatusCodeRules(row.status_codes).normalized
    if (codes !== '') override.status_codes = codes
    result[row.group.trim()] = override
  }
  return JSON.stringify(result)
}
