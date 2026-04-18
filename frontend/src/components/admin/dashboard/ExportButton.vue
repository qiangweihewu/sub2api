<template>
  <button type="button" class="btn btn-primary" @click="onClick">
    <svg class="mr-1.5 h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24" stroke-width="1.5">
      <path stroke-linecap="round" stroke-linejoin="round" d="M3 16.5v2.25A2.25 2.25 0 005.25 21h13.5A2.25 2.25 0 0021 18.75V16.5M16.5 12L12 16.5m0 0L7.5 12m4.5 4.5V3" />
    </svg>
    <span>Export CSV</span>
  </button>
</template>

<script setup lang="ts">
const props = defineProps<{
  scope: 'account' | 'group'
  id: number
  from: string
  to: string
}>()

// Convert an ISO timestamp to a local-date string "YYYY-MM-DD" to match
// the existing backend usage query date format.
function isoToLocalDate(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const y = d.getFullYear()
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${y}-${m}-${day}`
}

function onClick() {
  const scopeKey = props.scope === 'account' ? 'account_id' : 'group_id'
  const params = new URLSearchParams({
    [scopeKey]: String(props.id),
    start_date: isoToLocalDate(props.from),
    end_date: isoToLocalDate(props.to)
  })
  const url = `/api/v1/admin/usage.csv?${params.toString()}`
  window.open(url, '_blank')
}
</script>
