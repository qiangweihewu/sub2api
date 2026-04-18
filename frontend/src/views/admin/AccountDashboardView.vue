<template>
  <AppLayout>
    <div class="space-y-6">
      <!-- Header: title + subtitle + preset buttons -->
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 class="text-xl font-bold text-gray-900 dark:text-white">Account Dashboard</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">Account #{{ accountId }}</p>
        </div>
        <div class="flex items-center gap-2">
          <div class="flex rounded-lg border border-gray-200 dark:border-dark-600">
            <button
              v-for="preset in PRESETS"
              :key="preset.key"
              type="button"
              class="px-3 py-1.5 text-xs font-medium transition-colors first:rounded-l-lg last:rounded-r-lg"
              :class="activePreset === preset.key
                ? 'bg-primary-600 text-white'
                : 'text-gray-600 hover:bg-gray-100 dark:text-gray-300 dark:hover:bg-dark-700'"
              @click="applyPreset(preset.key)"
            >
              {{ preset.label }}
            </button>
          </div>
          <ExportButton scope="account" :id="accountId" :from="from" :to="to" />
        </div>
      </div>

      <!-- Stats -->
      <StatsOverview scope="account" :id="accountId" :from="from" :to="to" />

      <!-- IP Breakdown -->
      <IPBreakdownTable scope="account" :id="accountId" :from="from" :to="to" />

      <!-- User Breakdown -->
      <UserBreakdownTable scope="account" :id="accountId" :from="from" :to="to" />

      <!-- Recent Requests -->
      <RecentRequestsTable scope="account" :id="accountId" :from="from" :to="to" />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import StatsOverview from '@/components/admin/dashboard/StatsOverview.vue'
import IPBreakdownTable from '@/components/admin/dashboard/IPBreakdownTable.vue'
import UserBreakdownTable from '@/components/admin/dashboard/UserBreakdownTable.vue'
import RecentRequestsTable from '@/components/admin/dashboard/RecentRequestsTable.vue'
import ExportButton from '@/components/admin/dashboard/ExportButton.vue'

type PresetKey = '24h' | '7d' | '30d'

const PRESETS: { key: PresetKey; label: string; hours: number }[] = [
  { key: '24h', label: '24h', hours: 24 },
  { key: '7d', label: '7d', hours: 24 * 7 },
  { key: '30d', label: '30d', hours: 24 * 30 }
]

const route = useRoute()
const accountId = computed(() => Number(route.params.id))

const activePreset = ref<PresetKey>('24h')

function computeRange(hours: number): { from: string; to: string } {
  const now = new Date()
  const start = new Date(now.getTime() - hours * 60 * 60 * 1000)
  return { from: start.toISOString(), to: now.toISOString() }
}

const initial = computeRange(24)
const from = ref<string>(initial.from)
const to = ref<string>(initial.to)

function applyPreset(key: PresetKey) {
  const preset = PRESETS.find((p) => p.key === key)
  if (!preset) return
  activePreset.value = key
  const range = computeRange(preset.hours)
  from.value = range.from
  to.value = range.to
}
</script>
