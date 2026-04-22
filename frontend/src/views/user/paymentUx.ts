import { normalizeVisibleMethod } from '@/components/payment/paymentFlow'
import { extractApiErrorCode, extractApiErrorMetadata } from '@/utils/apiError'

const DISPLAY_METHOD_ALIASES: Record<string, string> = {
  wechat: 'wxpay',
  wechat_pay: 'wxpay',
}

export interface PaymentScenarioContext {
  paymentMethod: string
  isMobile: boolean
  isWechatBrowser: boolean
}

export interface PaymentScenarioErrorDescriptor {
  messageKey: string
  hintKey?: string
  /**
   * Raw upstream gateway error message (from metadata.upstream_error).
   * When set, callers should display it as a supplementary hint with a
   * "支付通道返回：" prefix so users see the real cause (e.g. "单笔限额超 2000"
   * from easypay, "cashapp does not support cny" from Stripe).
   */
  upstreamError?: string
}

export function normalizePaymentMethodForDisplay(paymentType: string): string {
  const trimmed = paymentType.trim().toLowerCase()
  const visibleMethod = normalizeVisibleMethod(trimmed)
  if (visibleMethod) return visibleMethod
  return DISPLAY_METHOD_ALIASES[trimmed] ?? trimmed
}

export function paymentMethodI18nKey(paymentType: string): string {
  return `payment.methods.${normalizePaymentMethodForDisplay(paymentType)}`
}

export function buildPaymentErrorToastMessage(message: string, hint?: string): string {
  if (!hint) return message
  return `${message} ${hint}`.trim()
}

function defaultWechatHint(context: PaymentScenarioContext): string {
  if (!context.isMobile) return 'payment.errors.wechatScanOnDesktopHint'
  return 'payment.errors.wechatOpenInWeChatHint'
}

function defaultAlipayHint(context: PaymentScenarioContext): string {
  if (context.isMobile) return 'payment.errors.alipayMobileOpenHint'
  return 'payment.errors.alipayDesktopQrHint'
}

export function describePaymentScenarioError(
  error: unknown,
  context: PaymentScenarioContext,
): PaymentScenarioErrorDescriptor | null {
  const method = normalizePaymentMethodForDisplay(context.paymentMethod)
  const code = extractApiErrorCode(error)
  const message = error instanceof Error
    ? error.message
    : (typeof error === 'object' && error && 'message' in error && typeof error.message === 'string'
      ? error.message
      : String(error || ''))
  const normalizedMessage = message.toLowerCase()
  const upstreamErrorRaw = extractApiErrorMetadata(error)?.upstream_error
  const upstreamError = typeof upstreamErrorRaw === 'string' && upstreamErrorRaw.trim()
    ? upstreamErrorRaw.trim()
    : undefined
  const withUpstream = (d: PaymentScenarioErrorDescriptor): PaymentScenarioErrorDescriptor =>
    upstreamError ? { ...d, upstreamError } : d

  if (method === 'wxpay') {
    if (code === 'WECHAT_H5_NOT_AUTHORIZED') {
      return withUpstream({
        messageKey: 'payment.errors.wechatH5NotAuthorized',
        hintKey: defaultWechatHint(context),
      })
    }
    if (code === 'WECHAT_PAYMENT_MP_NOT_CONFIGURED') {
      return withUpstream({
        messageKey: 'payment.errors.wechatPaymentMpNotConfigured',
        hintKey: context.isWechatBrowser
          ? 'payment.errors.wechatSwitchBrowserHint'
          : defaultWechatHint(context),
      })
    }
    if (code === 'NO_AVAILABLE_INSTANCE') {
      return withUpstream({
        messageKey: 'payment.errors.wechatUnavailable',
        hintKey: defaultWechatHint(context),
      })
    }
    if (code === 'WECHAT_JSAPI_FAILED' || normalizedMessage.includes('get_brand_wcpay_request:fail')) {
      return withUpstream({
        messageKey: 'payment.errors.wechatJsapiFailed',
        hintKey: defaultWechatHint(context),
      })
    }
    if (
      normalizedMessage.includes('weixinjsbridge is unavailable') ||
      normalizedMessage.includes('wechat_jsapi_unavailable')
    ) {
      return withUpstream({
        messageKey: 'payment.errors.wechatJsapiUnavailable',
        hintKey: 'payment.errors.wechatOpenInWeChatHint',
      })
    }
    if (code === 'PAYMENT_GATEWAY_ERROR' || code === 'UNHANDLED_PAYMENT_SCENARIO') {
      return withUpstream({
        messageKey: 'payment.errors.wechatUnavailable',
        hintKey: defaultWechatHint(context),
      })
    }
  }

  if (method === 'alipay' && (code === 'PAYMENT_GATEWAY_ERROR' || code === 'UNHANDLED_PAYMENT_SCENARIO')) {
    return withUpstream({
      messageKey: context.isMobile
        ? 'payment.errors.alipayMobileUnavailable'
        : 'payment.errors.alipayDesktopUnavailable',
      hintKey: defaultAlipayHint(context),
    })
  }

  // Generic gateway error for any other method (card / link / etc.): no specific i18n, just
  // show the upstream error if we have one so the user at least sees the real cause.
  if (code === 'PAYMENT_GATEWAY_ERROR' && upstreamError) {
    return {
      messageKey: 'payment.errors.genericGatewayError',
      upstreamError,
    }
  }

  return null
}
