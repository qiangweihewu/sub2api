<template>
  <div>
    <div v-if="error" class="mb-3 rounded-lg border border-red-200 bg-red-50 px-4 py-2 text-sm text-red-700 dark:border-red-800/40 dark:bg-red-900/20 dark:text-red-300">
      {{ error }}
    </div>
    <div class="grid grid-cols-2 gap-4 lg:grid-cols-3 xl:grid-cols-6">
      <div class="card p-4">
        <p class="text-xs font-medium text-gray-500 dark:text-gray-400">Requests</p>
        <p class="mt-1 text-xl font-bold text-gray-900 dark:text-white">{{ display(data?.request_count) }}</p>
      </div>
      <div class="card p-4">
        <p class="text-xs font-medium text-gray-500 dark:text-gray-400">Input Tokens</p>
        <p class="mt-1 text-xl font-bold text-gray-900 dark:text-white">{{ display(data?.input_tokens) }}</p>
      </div>
      <div class="card p-4">
        <p class="text-xs font-medium text-gray-500 dark:text-gray-400">Output Tokens</p>
        <p class="mt-1 text-xl font-bold text-gray-900 dark:text-white">{{ display(data?.output_tokens) }}</p>
      </div>
      <div class="card p-4">
        <p class="text-xs font-medium text-gray-500 dark:text-gray-400">Total Cost (USD)</p>
        <p class="mt-1 text-xl font-bold text-gray-900 dark:text-white">{{ displayCost(data?.total_cost_usd) }}</p>
      </div>
      <div class="card p-4">
        <p class="text-xs font-medium text-gray-500 dark:text-gray-400">Unique IPs</p>
        <p class="mt-1 text-xl font-bold text-gray-900 dark:text-white">{{ display(data?.unique_ips) }}</p>
      </div>
      <div class="card p-4">
        <p class="text-xs font-medium text-gray-500 dark:text-gray-400">Unique Users</p>
        <p class="mt-1 text-xl font-bold text-gray-900 dark:text-white">{{ display(data?.unique_users) }}</p>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { dashboardStatsApi, type Overview } from '@/api/admin/dashboardStats'

const props = defineProps<{
  scope: 'account' | 'group'
  id: number
  from: string
  to: string
}>()

const data = ref<Overview | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)

const display = (n: number | undefined): string => {
  if (loading.value) return '…'
  if (n === undefined || n === null) return '—'
  return n.toLocaleString()
}

const displayCost = (n: number | undefined): string => {
  if (loading.value) return '…'
  if (n === undefined || n === null) return '—'
  return n.toLocaleString(undefined, { minimumFractionDigits: 4, maximumFractionDigits: 4 })
}

async function load() {
  loading.value = true
  error.value = null
  try {
    const q = { from: props.from, to: props.to }
    const res =
      props.scope === 'account'
        ? await dashboardStatsApi.accountOverview(props.id, q)
        : await dashboardStatsApi.groupOverview(props.id, q)
    data.value = res
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Failed to load overview'
    data.value = null
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
