<template>
  <div class="card">
    <div class="flex items-center justify-between border-b border-gray-100 px-4 py-3 dark:border-dark-700">
      <h2 class="text-base font-semibold text-gray-900 dark:text-white">Recent Requests</h2>
    </div>
    <div class="p-4">
      <div v-if="error" class="mb-3 rounded-lg border border-red-200 bg-red-50 px-4 py-2 text-sm text-red-700 dark:border-red-800/40 dark:bg-red-900/20 dark:text-red-300">
        {{ error }}
      </div>
      <UsageTable
        :data="rows"
        :loading="loading"
        :columns="columns"
      />
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import UsageTable from '@/components/admin/usage/UsageTable.vue'
import { adminUsageAPI } from '@/api/admin/usage'
import type { AdminUsageLog } from '@/types'
import type { Column } from '@/components/common/types'

const props = defineProps<{
  scope: 'account' | 'group'
  id: number
  from: string
  to: string
  pageSize?: number
}>()

const { t } = useI18n()

const rows = ref<AdminUsageLog[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

const columns = computed<Column[]>(() => [
  { key: 'user', label: t('admin.usage.user'), sortable: false },
  { key: 'api_key', label: t('usage.apiKeyFilter'), sortable: false },
  { key: 'model', label: t('usage.model'), sortable: false },
  { key: 'endpoint', label: t('usage.endpoint'), sortable: false },
  { key: 'stream', label: t('usage.type'), sortable: false },
  { key: 'tokens', label: t('usage.tokens'), sortable: false },
  { key: 'cost', label: t('usage.cost'), sortable: false },
  { key: 'duration', label: t('usage.duration'), sortable: false },
  { key: 'created_at', label: t('usage.time'), sortable: false },
  { key: 'ip_address', label: t('admin.usage.ipAddress'), sortable: false }
])

// Convert an ISO timestamp like "2026-04-17T10:00:00.000Z" to a local-date "YYYY-MM-DD"
// This matches how UsageView computes its date range (via formatLD in local time).
function isoToLocalDate(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const y = d.getFullYear()
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${y}-${m}-${day}`
}

let abortCtl: AbortController | null = null

async function load() {
  abortCtl?.abort()
  const ctl = new AbortController()
  abortCtl = ctl
  loading.value = true
  error.value = null
  try {
    const params: Record<string, unknown> = {
      page: 1,
      page_size: props.pageSize ?? 20,
      exact_total: false,
      sort_by: 'created_at',
      sort_order: 'desc',
      start_date: isoToLocalDate(props.from),
      end_date: isoToLocalDate(props.to)
    }
    if (props.scope === 'account') {
      params.account_id = props.id
    } else {
      params.group_id = props.id
    }
    const res = await adminUsageAPI.list(params as any, { signal: ctl.signal })
    if (!ctl.signal.aborted) {
      rows.value = res.items
    }
  } catch (e: any) {
    if (e?.name === 'AbortError' || e?.name === 'CanceledError') return
    error.value = e instanceof Error ? e.message : 'Failed to load recent requests'
    rows.value = []
  } finally {
    if (abortCtl === ctl) loading.value = false
  }
}

watch(
  [() => props.scope, () => props.id, () => props.from, () => props.to],
  () => {
    void load()
  },
  { immediate: true }
)
</script>
