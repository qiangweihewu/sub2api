<template>
  <div class="card">
    <div class="flex items-center justify-between border-b border-gray-100 px-4 py-3 dark:border-dark-700">
      <h2 class="text-base font-semibold text-gray-900 dark:text-white">IP Access</h2>
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
              <th class="px-2 py-2 font-medium">IP</th>
              <th class="px-2 py-2 font-medium">Requests</th>
              <th class="px-2 py-2 font-medium">Users</th>
              <th class="px-2 py-2 font-medium">Input Tokens</th>
              <th class="px-2 py-2 font-medium">Output Tokens</th>
              <th class="px-2 py-2 font-medium">Last Seen</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="r in rows"
              :key="r.ip_address"
              class="border-t border-gray-100 text-gray-800 dark:border-dark-700 dark:text-gray-200"
            >
              <td class="px-2 py-2 font-mono">{{ r.ip_address }}</td>
              <td class="px-2 py-2">{{ r.request_count.toLocaleString() }}</td>
              <td class="px-2 py-2">{{ r.unique_users.toLocaleString() }}</td>
              <td class="px-2 py-2">{{ r.input_tokens.toLocaleString() }}</td>
              <td class="px-2 py-2">{{ r.output_tokens.toLocaleString() }}</td>
              <td class="px-2 py-2">{{ formatDate(r.last_seen_at) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { dashboardStatsApi, type IPBreakdownRow } from '@/api/admin/dashboardStats'

const props = defineProps<{
  scope: 'account' | 'group'
  id: number
  from: string
  to: string
}>()

const rows = ref<IPBreakdownRow[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

const formatDate = (s: string): string => {
  if (!s) return '—'
  const d = new Date(s)
  return isNaN(d.getTime()) ? s : d.toLocaleString()
}

async function load() {
  loading.value = true
  error.value = null
  try {
    const q = { from: props.from, to: props.to, limit: 100 }
    const res =
      props.scope === 'account'
        ? await dashboardStatsApi.accountIPs(props.id, q)
        : await dashboardStatsApi.groupIPs(props.id, q)
    rows.value = res.rows
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Failed to load IP breakdown'
    rows.value = []
  } finally {
    loading.value = false
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
