<template>
  <button
    type="button"
    class="btn btn-primary"
    :disabled="loading"
    @click="onClick"
  >
    <svg
      class="mr-1.5 h-4 w-4"
      :class="{ 'animate-spin': loading }"
      fill="none"
      stroke="currentColor"
      viewBox="0 0 24 24"
      stroke-width="1.5"
    >
      <path
        v-if="!loading"
        stroke-linecap="round"
        stroke-linejoin="round"
        d="M3 16.5v2.25A2.25 2.25 0 005.25 21h13.5A2.25 2.25 0 0021 18.75V16.5M16.5 12L12 16.5m0 0L7.5 12m4.5 4.5V3"
      />
      <path
        v-else
        stroke-linecap="round"
        stroke-linejoin="round"
        d="M12 3v3m0 12v3m9-9h-3M6 12H3m15.364-6.364l-2.121 2.121M7.757 16.243l-2.121 2.121m12.728 0l-2.121-2.121M7.757 7.757L5.636 5.636"
      />
    </svg>
    <span>{{ loading ? 'Exporting…' : 'Export CSV' }}</span>
  </button>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { downloadUsageCSV } from '@/api/admin/dashboardStats'

const props = defineProps<{
  scope: 'account' | 'group'
  id: number
  from: string
  to: string
}>()

const loading = ref(false)

// Convert an ISO timestamp to a local-date string "YYYY-MM-DD" to match the
// existing backend usage query date format.
function isoToLocalDate(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const y = d.getFullYear()
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${y}-${m}-${day}`
}

// axios + responseType:'blob' still returns a Blob when the server writes a
// JSON error body, so we read it here to surface the real message.
async function extractBlobErrorMessage(blob: Blob | undefined, fallback: string): Promise<string> {
  if (!blob || typeof blob.text !== 'function') return fallback
  try {
    const text = await blob.text()
    if (!text) return fallback
    try {
      const parsed = JSON.parse(text) as { message?: string; error?: string }
      return parsed.message || parsed.error || fallback
    } catch {
      return text.slice(0, 200) || fallback
    }
  } catch {
    return fallback
  }
}

async function onClick() {
  if (loading.value) return
  loading.value = true
  try {
    const { blob, filename } = await downloadUsageCSV({
      scope: props.scope,
      id: props.id,
      start_date: isoToLocalDate(props.from),
      end_date: isoToLocalDate(props.to)
    })

    // Derive a friendlier default name than the server's timestamped one so
    // users can quickly recognize the scope they exported.
    const today = new Date()
    const yyyymmdd =
      `${today.getFullYear()}` +
      `${String(today.getMonth() + 1).padStart(2, '0')}` +
      `${String(today.getDate()).padStart(2, '0')}`
    const suggested = `usage-${props.scope}-${props.id}-${yyyymmdd}.csv`
    const downloadName = filename && filename !== '' ? filename : suggested

    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = suggested || downloadName
    a.rel = 'noopener'
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    // Defer revoke so Safari has a chance to pick up the download.
    setTimeout(() => URL.revokeObjectURL(url), 0)
  } catch (err: unknown) {
    const e = err as { message?: string; data?: Blob; response?: { data?: Blob } }
    let message = e?.message || 'Export failed'
    // The response interceptor rejects with a structured object, but axios
    // itself may put the raw blob on `response.data` for non-2xx blob requests.
    const payload = e?.data ?? e?.response?.data
    if (payload instanceof Blob) {
      message = await extractBlobErrorMessage(payload, message)
    }
    // Use native alert as a minimal surface; the rest of the admin UI does the
    // same for one-off errors and no app-wide toast helper is imported here.
    window.alert(`Export failed: ${message}`)
  } finally {
    loading.value = false
  }
}
</script>
