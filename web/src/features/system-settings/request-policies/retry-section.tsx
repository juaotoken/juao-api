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
import { useQuery } from '@tanstack/react-query'
import { Plus, Trash2 } from 'lucide-react'
import { useFieldArray, useFormContext, useWatch } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import {
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
import { getGroups } from '@/features/users/api'
import { requireServerSuccess } from '@/lib/server-error-message'

import { SettingsCard } from '../components/settings-card'
import { safeNumberFieldProps } from '../utils/numeric-field'
import type { RoutingPolicyFormValues } from './routing-form'

export function RetrySection() {
  const { t } = useTranslation()
  const form = useFormContext<RoutingPolicyFormValues>()
  return (
    <SettingsCard title={t('Retry budget')} className='shadow-none'>
      <div className='space-y-4'>
        <FormField
          control={form.control}
          name='RetryTimes'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Maximum retries')}</FormLabel>
              <FormControl>
                <Input
                  type='number'
                  min={0}
                  max={99}
                  step={1}
                  {...safeNumberFieldProps(field)}
                />
              </FormControl>
              <FormDescription>
                {t('Excludes the first attempt. Counted per group.')}
              </FormDescription>
              <FormMessage />
            </FormItem>
          )}
        />
        <FormField
          control={form.control}
          name='AutomaticRetryStatusCodes'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Auto-retry status codes')}</FormLabel>
              <FormControl>
                <Input
                  {...field}
                  placeholder={t('e.g. 401, 403, 429, 500-599')}
                />
              </FormControl>
              <FormDescription>
                {t(
                  '2xx is always excluded. 504/524 follow the configured ranges.'
                )}
              </FormDescription>
              <FormMessage />
            </FormItem>
          )}
        />
        <GroupRetryOverrides />
      </div>
    </SettingsCard>
  )
}

// Per-group overrides of the retry budget above. Only pricing groups can be
// chosen; the auto group is excluded because it retries across groups and
// always follows the global settings.
function GroupRetryOverrides() {
  const { t } = useTranslation()
  const form = useFormContext<RoutingPolicyFormValues>()
  const overrides = useFieldArray({
    control: form.control,
    name: 'GroupRetryOverrides',
  })
  const rows = useWatch({ control: form.control, name: 'GroupRetryOverrides' })
  const groupsQuery = useQuery({
    queryKey: ['request-policy', 'pricing-groups'],
    queryFn: async () => {
      const groups = requireServerSuccess(await getGroups()).data
      return Array.isArray(groups) ? groups : []
    },
  })
  const groups = (groupsQuery.data ?? [])
    .filter((group) => group !== 'auto')
    .sort((a, b) => a.localeCompare(b))
  const configured = new Set((rows ?? []).map((row) => row.group))

  return (
    <div className='space-y-3'>
      <div className='flex items-start justify-between gap-3'>
        <div className='space-y-1'>
          <h4 className='text-sm font-medium'>
            {t('Per-group retry settings')}
          </h4>
          <p className='text-muted-foreground text-xs'>
            {t(
              'Override the retry count and status codes for specific pricing groups. Empty fields use the global values above. The auto group always uses the global settings.'
            )}
          </p>
        </div>
        <Button
          type='button'
          variant='outline'
          size='sm'
          onClick={() =>
            overrides.append({ group: '', retry_times: '', status_codes: '' })
          }
        >
          <Plus aria-hidden='true' data-icon='inline-start' />
          {t('Add group')}
        </Button>
      </div>
      {overrides.fields.map((item, index) => {
        const current = rows?.[index]?.group ?? ''
        const options = groups.filter(
          (group) => group === current || !configured.has(group)
        )
        // A group deleted from group pricing keeps its row so it can be removed.
        if (current !== '' && !options.includes(current)) {
          options.unshift(current)
        }
        return (
          <div
            key={item.id}
            className='grid gap-3 rounded-md border p-3 md:grid-cols-[minmax(0,1.2fr)_minmax(0,0.6fr)_minmax(0,1.4fr)_auto]'
          >
            <FormField
              control={form.control}
              name={`GroupRetryOverrides.${index}.group`}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Group')}</FormLabel>
                  <Select
                    items={options.map((group) => ({
                      value: group,
                      label: group,
                    }))}
                    value={field.value || null}
                    onValueChange={(value) => field.onChange(value ?? '')}
                  >
                    <FormControl>
                      <SelectTrigger className='w-full'>
                        <SelectValue placeholder={t('Select a group')} />
                      </SelectTrigger>
                    </FormControl>
                    <SelectContent alignItemWithTrigger={false}>
                      <SelectGroup>
                        {options.map((group) => (
                          <SelectItem key={group} value={group}>
                            {group}
                          </SelectItem>
                        ))}
                      </SelectGroup>
                    </SelectContent>
                  </Select>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name={`GroupRetryOverrides.${index}.retry_times`}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Maximum retries')}</FormLabel>
                  <FormControl>
                    <Input
                      {...field}
                      inputMode='numeric'
                      placeholder={t('Global')}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name={`GroupRetryOverrides.${index}.status_codes`}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Auto-retry status codes')}</FormLabel>
                  <FormControl>
                    <Input {...field} placeholder={t('Global')} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <div className='flex items-end'>
              <Button
                type='button'
                variant='ghost'
                size='icon'
                aria-label={t('Remove group {{group}}', {
                  group: current || index + 1,
                })}
                onClick={() => overrides.remove(index)}
              >
                <Trash2 aria-hidden='true' />
              </Button>
            </div>
          </div>
        )
      })}
    </div>
  )
}
