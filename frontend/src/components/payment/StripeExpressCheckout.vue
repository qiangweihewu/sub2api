<template>
  <div v-if="show" ref="containerRef" class="stripe-express-checkout-container"></div>
</template>

<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'

const props = defineProps<{
  publishableKey: string
  amountInCents: number
  currency: string
}>()

const emit = defineEmits<{
  // Emits the ECE confirm event + the elements/stripe instances so the parent
  // can create an order and confirm the payment.
  confirm: [payload: { event: any; elements: any; stripe: any }]
  ready: []
  error: [err: Error]
}>()

const show = ref(false)
const containerRef = ref<HTMLDivElement | null>(null)
// Keep references so we can unmount cleanly.
let stripeInstance: any = null
let elementsInstance: any = null
let expressCheckoutInstance: any = null

onMounted(async () => {
  if (!props.publishableKey || props.amountInCents <= 0) {
    return
  }
  try {
    const { loadStripe } = await import('@stripe/stripe-js')
    const stripe = await loadStripe(props.publishableKey)
    if (!stripe) {
      emit('error', new Error('Stripe.js failed to load'))
      return
    }
    stripeInstance = stripe

    const elements = stripe.elements({
      mode: 'payment',
      amount: props.amountInCents,
      currency: props.currency,
    } as any)
    elementsInstance = elements

    const expressCheckout = elements.create('expressCheckout' as any, {
      buttonHeight: 44,
    } as any)
    expressCheckoutInstance = expressCheckout

    show.value = true
    // Wait a microtask so v-if renders the container div.
    await new Promise<void>(resolve => setTimeout(resolve, 0))
    if (containerRef.value) {
      expressCheckout.mount(containerRef.value)
      ;(expressCheckout as any).on('ready', () => emit('ready'))
      ;(expressCheckout as any).on('confirm', (event: any) => {
        if (!stripeInstance || !elementsInstance) return
        emit('confirm', { event, elements: elementsInstance, stripe: stripeInstance })
      })
    }
  } catch (err) {
    emit('error', err as Error)
  }
})

onUnmounted(() => {
  if (expressCheckoutInstance) {
    try {
      expressCheckoutInstance.unmount()
    } catch {
      // ignore
    }
    expressCheckoutInstance = null
  }
  stripeInstance = null
  elementsInstance = null
})
</script>

<style scoped>
.stripe-express-checkout-container {
  margin-top: 12px;
}
</style>
