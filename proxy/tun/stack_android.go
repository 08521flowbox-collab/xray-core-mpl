//go:build android

package tun

import (
	"context"
)

// NewStack builds the built-in TCP/IP stack on the tun's descriptor
func NewStack(ctx context.Context, options StackOptions, handler *Handler) (Stack, error) {
	packets, err := options.Tun.(*AndroidTun).packets()
	if err != nil {
		return nil, err
	}
	return newLightStack(ctx, packets, options.MTU, options.IdleTimeout, handler), nil
}
