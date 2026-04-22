//go:build unit

package service

import (
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func TestClassifyCreatePaymentErrorReturnsNilOnNilInput(t *testing.T) {
	if err := classifyCreatePaymentError(CreateOrderRequest{}, payment.TypeStripe, nil); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestClassifyCreatePaymentErrorGatewayAttachesUpstreamMetadata(t *testing.T) {
	upstream := errors.New("单笔限额超 2000")
	req := CreateOrderRequest{PaymentType: payment.TypeAlipay}

	err := classifyCreatePaymentError(req, payment.TypeEasyPay, upstream)

	appErr, ok := err.(*infraerrors.ApplicationError)
	if !ok {
		t.Fatalf("expected *infraerrors.ApplicationError, got %T: %v", err, err)
	}
	if appErr.Reason != "PAYMENT_GATEWAY_ERROR" {
		t.Errorf("Reason = %q, want PAYMENT_GATEWAY_ERROR", appErr.Reason)
	}
	if appErr.Message != "payment gateway error" {
		t.Errorf("Message = %q, want 'payment gateway error'", appErr.Message)
	}
	if got := appErr.Metadata["upstream_error"]; got != "单笔限额超 2000" {
		t.Errorf("metadata[upstream_error] = %q, want '单笔限额超 2000'", got)
	}
	if got := appErr.Metadata["provider"]; got != payment.TypeEasyPay {
		t.Errorf("metadata[provider] = %q, want %q", got, payment.TypeEasyPay)
	}
}

func TestClassifyCreatePaymentErrorWxpayH5StillReturnsSpecificCode(t *testing.T) {
	upstream := errors.New("wxpay h5 payments are not authorized for this merchant")
	req := CreateOrderRequest{PaymentType: payment.TypeWxpay}

	err := classifyCreatePaymentError(req, payment.TypeWxpay, upstream)

	appErr, ok := err.(*infraerrors.ApplicationError)
	if !ok {
		t.Fatalf("expected *infraerrors.ApplicationError, got %T: %v", err, err)
	}
	if appErr.Reason != "WECHAT_H5_NOT_AUTHORIZED" {
		t.Errorf("Reason = %q, want WECHAT_H5_NOT_AUTHORIZED (H5 branch should take precedence over generic gateway error)", appErr.Reason)
	}
}
