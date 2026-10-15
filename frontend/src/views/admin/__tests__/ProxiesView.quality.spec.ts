import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount } from '@vue/test-utils'
import ProxiesView from '../ProxiesView.vue'

const { list, getAllWithCount, checkQuality, testProxy, showSuccess, showWarning } = vi.hoisted(() => ({
  list: vi.fn(), getAllWithCount: vi.fn(), checkQuality: vi.fn(), testProxy: vi.fn(), showSuccess: vi.fn(), showWarning: vi.fn()
}))
vi.mock('@/api/admin', () => ({ adminAPI: { proxies: { list, getAllWithCount, checkProxyQuality: checkQuality, testProxy } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess, showWarning }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string, params?: unknown) => params ? `${key} ${JSON.stringify(params)}` : key })
}))

const mountView = () => shallowMount(ProxiesView, {
  global: { stubs: {
    AppLayout: { template: '<div><slot /></div>' },
    TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /></div>' },
    DataTable: {
      props: ['data'],
      template: '<div v-for="row in data" :key="row.id"><slot name="cell-select" :row="row" /><section data-test="quality"><slot name="cell-latency" :row="row" /></section><slot name="cell-actions" :row="row" /></div>'
    },
    BaseDialog: { props: ['show'], template: '<div v-if="show" data-test="dialog"><slot /><slot name="footer" /></div>' }
  } }
})
let wrapper: ReturnType<typeof mountView>
beforeEach(() => {
  vi.clearAllMocks()
  list.mockResolvedValue({ items: [{
    id: 9, name: 'proxy', protocol: 'http', host: 'proxy.example', port: 8080,
    username: null, status: 'active', quality_checked: 1, quality_status: 'unknown',
    quality_grade: 'unknown', quality_score: 0
  }], total: 1, pages: 1 })
  getAllWithCount.mockResolvedValue([])
})
afterEach(() => wrapper?.unmount())

describe('proxy quality evidence presentation', () => {
  it.each(['unknown', 'api_error'])('does not report an inconclusive batch as healthy: %s', async (outcome) => {
    if (outcome === 'api_error') checkQuality.mockRejectedValue(new Error('synthetic API unavailable'))
    else checkQuality.mockResolvedValue({ proxy_id: 9, score: 0, grade: 'unknown', summary: 'no evidence',
      passed_count: 0, failed_count: 0, warn_count: 0, challenge_count: 0,
      unknown_count: 1, overall_status: 'unknown', checked_at: 2, items: [] })
    wrapper = mountView()
    await flushPromises()
    await wrapper.get('input[type="checkbox"]').setValue(true)
    await wrapper.get('button[title="admin.proxies.batchQualityCheck"]').trigger('click')
    await flushPromises()
    expect(showSuccess).not.toHaveBeenCalled()
    expect(showWarning).toHaveBeenCalledWith(expect.stringContaining('"unknown":1'))
    expect(showWarning).toHaveBeenCalledWith(expect.stringContaining('"healthy":0'))
    expect(showWarning).toHaveBeenCalledWith(expect.stringContaining('"failed":0'))
  })

  it('does not turn an admin API failure into a failed proxy observation', async () => {
    list.mockResolvedValue({ items: [{ id: 9, name: 'proxy', protocol: 'http', host: 'proxy.example', port: 8080,
      username: null, status: 'active', latency_status: 'success', latency_ms: 31 }], total: 1, pages: 1 })
    testProxy.mockRejectedValue(new Error('synthetic admin API unavailable'))
    wrapper = mountView()
    await flushPromises()
    await wrapper.findAll('button').find(button => !button.attributes('title') && button.text().includes('admin.proxies.testConnection'))!.trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="quality"]').text()).toContain('31ms')
    expect(wrapper.get('[data-test="quality"]').text()).not.toContain('admin.proxies.latencyFailed')
  })

  it('keeps the last measured latency when a new connection check is inconclusive', async () => {
    list.mockResolvedValue({ items: [{ id: 9, name: 'proxy', protocol: 'http', host: 'proxy.example', port: 8080,
      username: null, status: 'active', latency_status: 'success', latency_ms: 23 }], total: 1, pages: 1 })
    testProxy.mockResolvedValue({ success: false, status: 'unknown', message: 'probe unavailable' })
    wrapper = mountView()
    await flushPromises()
    await wrapper.findAll('button').find(button => !button.attributes('title') && button.text().includes('admin.proxies.testConnection'))!.trigger('click')
    await flushPromises()
    const quality = wrapper.get('[data-test="quality"]')
    expect(quality.text()).toContain('23ms')
    expect(quality.text()).not.toContain('admin.proxies.latencyFailed')
  })

  it('does not label missing evidence as a failed proxy', async () => {
    wrapper = mountView()
    await flushPromises()
    const quality = wrapper.get('[data-test="quality"]')
    expect(quality.text()).toContain('admin.proxies.qualityStatusUnknown')
    expect(quality.text()).not.toContain('admin.proxies.qualityStatusFail')
    expect(quality.find('.badge-danger').exists()).toBe(false)
  })

  it('keeps unknown and inapplicable results distinct after a live quality check', async () => {
    checkQuality.mockResolvedValue({
      proxy_id: 9, score: 100, grade: 'unknown', summary: 'incomplete evidence',
      passed_count: 1, failed_count: 0, warn_count: 0, challenge_count: 0,
      unknown_count: 1, not_applicable_count: 1, overall_status: 'unknown', checked_at: 2,
      items: [
        { target: 'openai', status: 'pass', http_status: 401 },
        { target: 'anthropic', status: 'unknown', reason_code: 'probe_unavailable' },
        { target: 'gemini', status: 'not_applicable' }
      ]
    })
    wrapper = mountView()
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text().includes('admin.proxies.qualityCheck'))!.trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="quality"]').text()).toContain('admin.proxies.qualityStatusUnknown')
    const dialog = wrapper.get('[data-test="dialog"]')
    expect(dialog.text()).toContain('admin.proxies.qualityStatusUnknown')
    expect(dialog.text()).toContain('admin.proxies.qualityStatusNotApplicable')
    expect(dialog.text()).not.toContain('admin.proxies.qualityStatusHealthy')
    expect(dialog.find('.text-2xl').text()).toBe('-')
  })
})
