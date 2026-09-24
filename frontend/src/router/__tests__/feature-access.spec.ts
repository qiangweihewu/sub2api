import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

type NavigationGuard = (
  to: Record<string, any>,
  from: Record<string, any>,
  next: ReturnType<typeof vi.fn>
) => Promise<void>

const routerHarness = vi.hoisted(() => ({
  guard: null as NavigationGuard | null,
}))

const authStore = vi.hoisted(() => ({
  checkAuth: vi.fn(),
  isAuthenticated: true,
  isAdmin: false,
  isReadonlyAdmin: false,
  canAccessAdminPanel: false,
  isSimpleMode: false,
  hasPendingAuthSession: false,
}))

const appStore = vi.hoisted(() => ({
  siteName: 'Sub2API',
  backendModeEnabled: false,
  publicSettingsLoaded: false,
  cachedPublicSettings: null as null | {
    payment_enabled?: boolean
    risk_control_enabled?: boolean
    subscription_enabled?: boolean
    custom_menu_items?: []
  },
  fetchPublicSettings: vi.fn(),
}))

vi.mock('vue-router', () => ({
  createWebHistory: vi.fn(() => ({})),
  createRouter: vi.fn(() => ({
    beforeEach: vi.fn((guard: NavigationGuard) => {
      routerHarness.guard = guard
    }),
    afterEach: vi.fn(),
    onError: vi.fn(),
  })),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => authStore,
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => appStore,
}))

vi.mock('@/stores/adminSettings', () => ({
  useAdminSettingsStore: () => ({ customMenuItems: [] }),
}))

vi.mock('@/stores/adminCompliance', () => ({
  useAdminComplianceStore: () => ({
    initialized: true,
    fetchStatus: vi.fn(),
    requireAcknowledgement: vi.fn(),
  }),
}))

vi.mock('@/composables/useNavigationLoading', () => ({
  useNavigationLoadingState: () => ({
    startNavigation: vi.fn(),
    endNavigation: vi.fn(),
    isLoading: { value: false },
  }),
}))

vi.mock('@/composables/useRoutePrefetch', () => ({
  useRoutePrefetch: () => ({
    triggerPrefetch: vi.fn(),
    cancelPendingPrefetch: vi.fn(),
    resetPrefetchState: vi.fn(),
  }),
}))

function createDeferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise
  })
  return { promise, resolve }
}

function runGuard(meta: Record<string, unknown>, path: string) {
  if (!routerHarness.guard) {
    throw new Error('router guard was not registered')
  }

  const next = vi.fn()
  const navigation = routerHarness.guard(
    {
      path,
      fullPath: path,
      name: 'FeatureRoute',
      params: {},
      meta: { requiresAuth: true, ...meta },
    },
    {},
    next
  )
  return { navigation, next }
}

describe('feature route guard', () => {
  beforeAll(async () => {
    await import('@/router')
  })

  beforeEach(() => {
    authStore.isAuthenticated = true
    authStore.isAdmin = false
    authStore.isReadonlyAdmin = false
    authStore.canAccessAdminPanel = false
    authStore.isSimpleMode = false
    appStore.backendModeEnabled = false
    appStore.publicSettingsLoaded = false
    appStore.cachedPublicSettings = null
    appStore.fetchPublicSettings.mockReset()
  })

  it('waits for the first public-settings request before deciding payment access', async () => {
    const deferred = createDeferred<{ payment_enabled: boolean }>()
    appStore.fetchPublicSettings.mockImplementation(async () => {
      const settings = await deferred.promise
      appStore.cachedPublicSettings = settings
      appStore.publicSettingsLoaded = true
      return settings
    })

    const { navigation, next } = runGuard({ requiresPayment: true }, '/purchase')

    await vi.waitFor(() => expect(appStore.fetchPublicSettings).toHaveBeenCalledTimes(1))
    expect(next).not.toHaveBeenCalled()

    deferred.resolve({ payment_enabled: true })
    await navigation
    expect(next).toHaveBeenCalledOnce()
    expect(next).toHaveBeenCalledWith()
  })

  it.each([
    ['payment', { requiresPayment: true }, '/purchase'],
    ['risk control', { requiresRiskControl: true }, '/admin/risk-control'],
    ['subscription', { requiresSubscription: true }, '/subscriptions'],
  ])('does not treat a failed %s settings load as explicitly disabled', async (_name, meta, path) => {
    authStore.isAdmin = meta.requiresRiskControl === true
    appStore.fetchPublicSettings.mockResolvedValue(null)

    const { navigation, next } = runGuard(meta, path)
    await navigation

    expect(appStore.publicSettingsLoaded).toBe(false)
    expect(next).toHaveBeenCalledOnce()
    expect(next).toHaveBeenCalledWith()
  })

  it.each([
    ['payment', { requiresPayment: true }, { payment_enabled: false }, '/dashboard'],
    [
      'risk control',
      { requiresRiskControl: true },
      { risk_control_enabled: false },
      '/admin/settings',
    ],
    ['subscription', { requiresSubscription: true }, { subscription_enabled: false }, '/dashboard'],
  ])('redirects when loaded settings explicitly disable %s', async (_name, meta, settings, target) => {
    authStore.isAdmin = meta.requiresRiskControl === true
    appStore.cachedPublicSettings = settings
    appStore.publicSettingsLoaded = true

    const { navigation, next } = runGuard(meta, '/feature')
    await navigation

    expect(appStore.fetchPublicSettings).not.toHaveBeenCalled()
    expect(next).toHaveBeenCalledOnce()
    expect(next).toHaveBeenCalledWith(target)
  })

  describe('backend mode admin panel access', () => {
    // readonly_admin is NOT supported in backend mode: the backend auth handlers
    // (backend/internal/handler/auth_handler.go Login/RefreshToken,
    // backend/internal/handler/passkey_handler.go) gate backend-mode login/2FA/passkey
    // and token refresh on user.IsAdmin() / UserRole=="admin", so the role can never
    // actually establish or keep a session when backend mode is on. The router must
    // fail closed here too — this is a regression test for the backend-mode gates in
    // @/router staying on isAdmin, not canAccessAdminPanel.
    it('blocks readonly_admin from a whitelisted admin page in backend mode, redirecting to /login', async () => {
      authStore.isAdmin = false
      authStore.isReadonlyAdmin = true
      authStore.canAccessAdminPanel = true
      appStore.backendModeEnabled = true

      const { navigation, next } = runGuard({ requiresAdmin: true }, '/admin/accounts')
      await navigation

      expect(next).toHaveBeenCalledOnce()
      expect(next).toHaveBeenCalledWith('/login')
    })

    it('still lets a real admin reach admin pages', async () => {
      authStore.isAdmin = true
      authStore.isReadonlyAdmin = false
      authStore.canAccessAdminPanel = true
      appStore.backendModeEnabled = true

      const { navigation, next } = runGuard({ requiresAdmin: true }, '/admin/dashboard')
      await navigation

      expect(next).toHaveBeenCalledOnce()
      expect(next).toHaveBeenCalledWith()
    })

    it('still blocks a plain user, redirecting to /login', async () => {
      authStore.isAdmin = false
      authStore.isReadonlyAdmin = false
      authStore.canAccessAdminPanel = false
      appStore.backendModeEnabled = true

      const { navigation, next } = runGuard({}, '/dashboard')
      await navigation

      expect(next).toHaveBeenCalledOnce()
      expect(next).toHaveBeenCalledWith('/login')
    })

    it('redirects readonly_admin off a non-whitelisted admin page to its home', async () => {
      authStore.isAdmin = false
      authStore.isReadonlyAdmin = true
      authStore.canAccessAdminPanel = true
      appStore.backendModeEnabled = false

      const { navigation, next } = runGuard({ requiresAdmin: true }, '/admin/settings')
      await navigation

      expect(next).toHaveBeenCalledOnce()
      expect(next).toHaveBeenCalledWith('/admin/accounts')
    })

    // Proves the revert above only removed backend-mode support and did not touch the
    // general requiresAdmin admission gate: with backend mode OFF, readonly_admin must
    // still reach a whitelisted admin page normally.
    it('still lets readonly_admin reach a whitelisted admin page when backend mode is off', async () => {
      authStore.isAdmin = false
      authStore.isReadonlyAdmin = true
      authStore.canAccessAdminPanel = true
      appStore.backendModeEnabled = false

      const { navigation, next } = runGuard({ requiresAdmin: true }, '/admin/accounts')
      await navigation

      expect(next).toHaveBeenCalledOnce()
      expect(next).toHaveBeenCalledWith()
    })
  })
})

describe('subscription route guard (opt-out flag)', () => {
  beforeEach(() => {
    authStore.isAdmin = false
    authStore.isSimpleMode = false
    appStore.publicSettingsLoaded = true
    appStore.fetchPublicSettings.mockReset()
  })

  it.each([
    ['missing key', {}],
    ['explicit true', { subscription_enabled: true }],
  ])('lets /subscriptions through when the flag is %s', async (_name, settings) => {
    appStore.cachedPublicSettings = settings

    const { navigation, next } = runGuard({ requiresSubscription: true }, '/subscriptions')
    await navigation

    expect(next).toHaveBeenCalledOnce()
    expect(next).toHaveBeenCalledWith()
  })

  it('sends admins to the admin dashboard when subscriptions are disabled', async () => {
    authStore.isAdmin = true
    appStore.cachedPublicSettings = { subscription_enabled: false }

    const { navigation, next } = runGuard({ requiresSubscription: true }, '/subscriptions')
    await navigation

    expect(next).toHaveBeenCalledWith('/admin/dashboard')
  })
})
