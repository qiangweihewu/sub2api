import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'
import { isReadonlyAdminPathAllowed } from '@/router/readonlyAdminPaths'

const componentPath = resolve(dirname(fileURLToPath(import.meta.url)), '../AppSidebar.vue')
const componentSource = readFileSync(componentPath, 'utf8')
const stylePath = resolve(dirname(fileURLToPath(import.meta.url)), '../../../style.css')
const styleSource = readFileSync(stylePath, 'utf8')

describe('AppSidebar custom SVG styles', () => {
  it('does not override uploaded SVG fill or stroke colors', () => {
    expect(componentSource).toContain('.sidebar-svg-icon {')
    expect(componentSource).toContain('color: currentColor;')
    expect(componentSource).toContain('display: block;')
    expect(componentSource).not.toContain('stroke: currentColor;')
    expect(componentSource).not.toContain('fill: none;')
  })
})

describe('AppSidebar scroll position persistence', () => {
  it('binds a template ref to the sidebar nav element', () => {
    expect(componentSource).toContain('ref="sidebarNavRef"')
    expect(componentSource).toContain('sidebar-nav')
  })

  it('declares sidebarNavRef in script setup', () => {
    expect(componentSource).toContain("const sidebarNavRef = ref<HTMLElement | null>(null)")
  })

  it('saves scroll position on beforeUnmount', () => {
    expect(componentSource).toContain('onBeforeUnmount')
    expect(componentSource).toContain('appStore.sidebarScrollTop')
    expect(componentSource).toContain('sidebarNavRef.value.scrollTop')
  })

  it('restores scroll position on mount', () => {
    expect(componentSource).toContain('onMounted')
    expect(componentSource).toContain('appStore.sidebarScrollTop')
    expect(componentSource).toContain('nextTick')
  })
})

describe('AppSidebar collapsible groups', () => {
  it('lets the user collapse a group even while a child route is active', () => {
    // The expand state must come from the user's override first, falling back
    // to the active-route heuristic only when the user has not clicked yet.
    expect(componentSource).toContain('const groupExpandOverrides = ref<Map<string, boolean>>(new Map())')
    expect(componentSource).not.toContain('expandedGroups.value.has(item.path) || isGroupActive(item)')
  })
})

describe('AppSidebar header styles', () => {
  it('does not clip the version badge dropdown', () => {
    const sidebarHeaderBlockMatch = styleSource.match(/\.sidebar-header\s*\{[\s\S]*?\n {2}\}/)
    const sidebarBrandBlockMatch = componentSource.match(/\.sidebar-brand\s*\{[\s\S]*?\n\}/)

    expect(sidebarHeaderBlockMatch).not.toBeNull()
    expect(sidebarBrandBlockMatch).not.toBeNull()
    expect(sidebarHeaderBlockMatch?.[0]).not.toContain('@apply overflow-hidden;')
    expect(sidebarBrandBlockMatch?.[0]).not.toContain('overflow: hidden;')
  })
})

// readonly_admin sidebar trimming. AppSidebar.vue is heavy to mount (many
// stores + inline SVG icon components), so — consistent with the rest of
// this file — we assert against the component source rather than mounting.
// The functional behavior of the filter itself (which items survive) is
// verified separately by replaying the same isReadonlyAdminPathAllowed logic
// against a snapshot of the real baseItems list (see task report).
describe('AppSidebar readonly_admin gating', () => {
  it('gates the admin nav template on canAccessAdminPanel, not isAdmin', () => {
    expect(componentSource).toContain('<template v-if="canAccessAdminPanel">')
    expect(componentSource).not.toContain('<template v-if="isAdmin">')
  })

  it('imports the shared readonly-admin allowlist helpers instead of duplicating them', () => {
    expect(componentSource).toContain(
      "import { isReadonlyAdminPathAllowed, READONLY_ADMIN_HOME } from '@/router/readonlyAdminPaths'"
    )
  })

  it('routes readonly_admin home to READONLY_ADMIN_HOME, not /admin/dashboard', () => {
    const homePathMatch = componentSource.match(/const homePath = computed\(\(\) => \{[\s\S]*?\n\}\)/)
    expect(homePathMatch).not.toBeNull()
    expect(homePathMatch?.[0]).toContain('if (isAdmin.value) return \'/admin/dashboard\'')
    expect(homePathMatch?.[0]).toContain('if (isReadonlyAdmin.value) return READONLY_ADMIN_HOME')
  })

  it('applies the readonly_admin filter after both /admin/settings pushes, at the end of adminNavItems', () => {
    const settingsPushIndices = [...componentSource.matchAll(/path: '\/admin\/settings'/g)].map((m) => m.index!)
    const filterIndex = componentSource.indexOf('if (isReadonlyAdmin.value) {')

    // Both the simple-mode branch and the full-mode branch push /admin/settings
    // unconditionally, so there must be exactly two occurrences.
    expect(settingsPushIndices).toHaveLength(2)
    expect(filterIndex).toBeGreaterThan(-1)
    for (const idx of settingsPushIndices) {
      expect(filterIndex).toBeGreaterThan(idx)
    }
  })

  it('filters both top-level items and nested children through isReadonlyAdminPathAllowed', () => {
    const filterBlockMatch = componentSource.match(/if \(isReadonlyAdmin\.value\) \{[\s\S]*?\n {2}\}/)
    expect(filterBlockMatch).not.toBeNull()
    expect(filterBlockMatch?.[0]).toContain('.filter((item) => isReadonlyAdminPathAllowed(item.path))')
    expect(filterBlockMatch?.[0]).toContain(
      'item.children.filter((c) => isReadonlyAdminPathAllowed(c.path))'
    )
  })

  // Functional replay: this reproduces the exact .filter()/.map() shape used by
  // adminNavItems (asserted structurally above) against the REAL
  // isReadonlyAdminPathAllowed from readonlyAdminPaths.ts — not a hand-copied
  // reimplementation — using a fixture that mirrors the real /admin/channels
  // baseItems entry (pricing + monitor children). This is what actually proves
  // the resulting menu shape, since the source-string tests above only prove
  // the wiring exists.
  it('resulting menu: keeps Channels + Pricing, drops the Monitor child', () => {
    type NavItem = { path: string; children?: NavItem[] }
    const adminChannelsItem: NavItem = {
      path: '/admin/channels',
      children: [
        { path: '/admin/channels/pricing' },
        { path: '/admin/channels/monitor' }
      ]
    }
    const baseItems: NavItem[] = [
      { path: '/admin/dashboard' },
      { path: '/admin/ops' },
      { path: '/admin/users' },
      { path: '/admin/groups' },
      adminChannelsItem,
      { path: '/admin/subscriptions' },
      { path: '/admin/accounts' },
      { path: '/admin/settings' }
    ]

    const result = baseItems
      .filter((item) => isReadonlyAdminPathAllowed(item.path))
      .map((item) =>
        item.children
          ? { ...item, children: item.children.filter((c) => isReadonlyAdminPathAllowed(c.path)) }
          : item
      )

    expect(result.map((i) => i.path)).toEqual(['/admin/ops', '/admin/users', '/admin/groups', '/admin/channels', '/admin/accounts'])
    const channels = result.find((i) => i.path === '/admin/channels')
    expect(channels?.children?.map((c) => c.path)).toEqual(['/admin/channels/pricing'])
  })
})
