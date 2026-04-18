<template>
  <div class="card">
    <div class="flex items-center justify-between border-b border-gray-100 px-4 py-3 dark:border-dark-700">
      <h2 class="text-base font-semibold text-gray-900 dark:text-white">Per-Account Split (LRU balance)</h2>
    </div>
    <div class="p-4">
      <div v-if="error" class="mb-3 rounded-lg border border-red-200 bg-red-50 px-4 py-2 text-sm text-red-700 dark:border-red-800/40 dark:bg-red-900/20 dark:text-red-300">
        {{ error }}
      </div>
      <div v-if="loading" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">Loading…</div>
      <div v-else-if="rows.length === 0" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">No data</div>
      <div v-else class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="text-left text-gray-500 dark:text-gray-400">
              <th class="px-2 py-2 font-medium">Account</th>
              <th class="px-2 py-2 font-medium">Requests</th>
              <th class="px-2 py-2 font-medium">Share</th>
              <th class="px-2 py-2 font-medium">Input Tokens</th>
              <th class="px-2 py-2 font-medium">Cost (USD)</th>
              <th class="px-2 py-2 font-medium">Last Seen</th>
              <th class="px-2 py-2 font-medium"></th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="r in rows"
              :key="r.account_id"
              class="border-t border-gray-100 text-gray-800 dark:border-dark-700 dark:text-gray-200"
            >
              <td class="px-2 py-2 font-mono">#{{ r.account_id }}</td>
              <td class="px-2 py-2">{{ r.request_count.toLocaleString() }}</td>
              <td class="px-2 py-2">{{ share(r.request_count) }}%</td>
              <td class="px-2 py-2">{{ r.input_tokens.toLocaleString() }}</td>
              <td class="px-2 py-2">{{ r.total_cost_usd.toFixed(4) }}</td>
              <td class="px-2 py-2">{{ formatDate(r.last_seen_at) }}</td>
              <td class="px-2 py-2">
                <router-link
                  :to="`/admin/accounts/${r.account_id}/dashboard`"
                  class="text-primary-600 hover:text-primary-700 dark:text-primary-400 dark:hover:text-primary-300"
                >
                  Dashboard
                </router-link>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { dashboardStatsApi, type AccountBreakdownRow } from '@/api/admin/dashboardStats'

const props = defineProps<{
  groupId: number
  from: string
  to: string
}>()

const rows = ref<AccountBreakdownRow[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

const totalRequests = computed(() => rows.value.reduce((s, r) => s + r.request_count, 0))

const share = (n: number): number => {
  const total = totalRequests.value
  if (total <= 0) return 0
  return Math.round((n / total) * 100)
}

const formatDate = (s: string): string => {
  if (!s) return '—'
  const d = new Date(s)
  return isNaN(d.getTime()) ? s : d.toLocaleString()
}

async function load() {
  loading.value = true
  error.value = null
  try {
    const res = await dashboardStatsApi.groupAccounts(props.groupId, {
      from: props.from,
      to: props.to
    })
    rows.value = res.rows
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Failed to load per-account breakdown'
    rows.value = []
  } finally {
    loading.value = false
  }
}

watch(
  [() => props.groupId, () => props.from, () => props.to],
  () => {
    void load()
  },
  { immediate: true }
)
</script>
